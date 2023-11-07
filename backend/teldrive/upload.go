package teldrive

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/gofrs/uuid"
	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/lib/rest"

	"github.com/rclone/rclone/fs"
)

type uploadInfo struct {
	existingChunks []api.PartFile
	uploadID       string
}

type objectChunkWriter struct {
	chunkSize       int64
	size            int64
	f               *Fs
	uploadID        string
	src             fs.ObjectInfo
	partsToCommitMu sync.Mutex
	partsToCommit   []api.PartFile
	existingParts   map[int]api.PartFile
	o               *Object
	totalParts      int64
}

// WriteChunk will write chunk number with reader bytes, where chunk number >= 0
func (w *objectChunkWriter) WriteChunk(ctx context.Context, chunkNumber int, reader io.ReadSeeker) (size int64, err error) {

	token := w.f.workers.Next()
	client, _ := BotClient(w.f, token)
	channelUser := strings.Split(token, ":")[0]
	if chunkNumber < 0 {
		err := fmt.Errorf("invalid chunk number provided: %v", chunkNumber)
		return -1, err
	}

	chunkNumber += 1

	if existing, ok := w.existingParts[chunkNumber]; ok {
		io.CopyN(io.Discard, reader, existing.Size)
		w.addCompletedPart(existing)
		return existing.Size, nil
	}

	var response api.PartFile

	err = w.f.pacer.Call(func() (bool, error) {

		size, err = reader.Seek(0, io.SeekEnd)
		if err != nil {

			return false, err
		}

		_, err = reader.Seek(0, io.SeekStart)
		if err != nil {
			return false, err
		}

		fs.Debugf(w.o, "Sending chunk %d length %d", chunkNumber, size)
		var name string
		if w.f.opt.RandomisePart {
			u1, _ := uuid.NewV4()
			name = hex.EncodeToString(u1.Bytes())
		} else {
			_, name = w.f.splitPathFull(w.src.Remote())
			if w.totalParts > 1 {
				name = fmt.Sprintf("%s.part.%03d", name, chunkNumber)
			}
		}

		params := UploadParams{
			Client:      client,
			PartNo:      chunkNumber,
			Name:        name,
			Body:        reader,
			Size:        size,
			Token:       token,
			ChannelUser: channelUser,
			ChannelID:   w.f.opt.ChannelID,
		}

		msgId, err := UploadFile(ctx, params)

		if err != nil {
			fs.Debugf(w.o, "Error sending chunk %d (retry=%v): %v: %#v", chunkNumber, true, err, err)
			return true, err
		}

		opts := rest.Opts{
			Method: "POST",
			Path:   "/api/uploads/parts",
		}
		payload := api.UploadPart{
			Name:      name,
			UploadId:  w.uploadID,
			PartId:    msgId,
			PartNo:    chunkNumber,
			ChannelID: w.f.opt.ChannelID,
			Size:      size,
		}
		w.f.pacer.Call(func() (bool, error) {
			resp, err := w.f.srv.CallJSON(ctx, &opts, &payload, nil)
			return shouldRetry(ctx, resp, err)
		})

		response.Name = name
		response.PartId = msgId
		response.PartNo = chunkNumber
		response.Size = size
		return false, nil

	})

	if err != nil {
		fs.Debugf(w.o, "Error sending chunk %d: %v", chunkNumber, err)
	} else {
		if response.PartId > 0 {
			w.addCompletedPart(response)
		}
		fs.Debugf(w.o, "Done sending chunk %d", chunkNumber)
	}
	return size, err

}

// add a part number and etag to the completed parts
func (w *objectChunkWriter) addCompletedPart(part api.PartFile) {
	w.partsToCommitMu.Lock()
	defer w.partsToCommitMu.Unlock()
	w.partsToCommit = append(w.partsToCommit, part)
}

func (w *objectChunkWriter) Close(ctx context.Context) error {

	if w.totalParts != int64(len(w.partsToCommit)) {
		return fmt.Errorf("uploaded failed")
	}

	base, leaf := w.f.splitPathFull(w.src.Remote())

	if base != "/" {
		err := w.f.CreateDir(ctx, base, "")
		if err != nil {
			return err
		}
	}
	opts := rest.Opts{
		Method: "POST",
		Path:   "/api/files",
	}

	sort.Slice(w.partsToCommit, func(i, j int) bool {
		return w.partsToCommit[i].PartNo < w.partsToCommit[j].PartNo
	})

	fileParts := []api.FilePart{}

	for _, part := range w.partsToCommit {
		fileParts = append(fileParts, api.FilePart{ID: part.PartId})
	}

	payload := api.CreateFileRequest{
		Name:     w.f.opt.Enc.FromStandardName(leaf),
		Type:     "file",
		Path:     base,
		MimeType: fs.MimeType(ctx, w.src),
		Size:     w.src.Size(),
		Parts:    fileParts,
	}

	err := w.f.pacer.Call(func() (bool, error) {
		resp, err := w.f.srv.CallJSON(ctx, &opts, &payload, nil)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return err
	}

	opts = rest.Opts{
		Method: "DELETE",
		Path:   "/api/uploads/" + w.uploadID,
	}
	err = w.f.pacer.Call(func() (bool, error) {
		resp, err := w.f.srv.Call(ctx, &opts)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return err
	}
	return nil
}

func (*objectChunkWriter) Abort(ctx context.Context) error {
	return nil
}

func (fs *Fs) prepareUpload(ctx context.Context, src fs.ObjectInfo) (*uploadInfo, error) {

	if len(fs.workers.bots) == 0 {
		var bots []string
		opts := rest.Opts{
			Method: "GET",
			Path:   "/api/users/bots",
		}
		err := fs.pacer.Call(func() (bool, error) {
			resp, err := fs.srv.CallJSON(ctx, &opts, nil, &bots)
			return shouldRetry(ctx, resp, err)
		})
		if err != nil {
			return nil, err
		}
		fs.workers.Set(bots)
	}

	base, leaf := fs.splitPathFull(src.Remote())

	modTime := src.ModTime(ctx).UTC().Format(timeFormat)

	uploadID := MD5(fmt.Sprintf("%s:%d:%s", path.Join(base, leaf), src.Size(), modTime))

	var uploadParts api.UploadFile
	opts := rest.Opts{
		Method: "GET",
		Path:   "/api/uploads/" + uploadID,
	}

	err := fs.pacer.Call(func() (bool, error) {
		resp, err := fs.srv.CallJSON(ctx, &opts, nil, &uploadParts)
		return shouldRetry(ctx, resp, err)
	})

	if err != nil {
		return nil, err
	}

	return &uploadInfo{existingChunks: uploadParts.Parts, uploadID: uploadID}, nil
}
