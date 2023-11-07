package teldrive

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	tdclock "github.com/gotd/td/clock"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/rclone/rclone/lib/kv"
	"golang.org/x/time/rate"
)

var ErrNotFound = errors.New("no record")

type UploadParams struct {
	PartNo      int
	Name        string
	Body        io.Reader
	Client      *telegram.Client
	Token       string
	ChannelID   int64
	ChannelUser string
	Size        int64
}

type BotWorkers struct {
	sync.Mutex
	bots  []string
	index int
}

func (w *BotWorkers) Set(bots []string) {
	w.Lock()
	defer w.Unlock()
	if len(w.bots) == 0 {
		w.bots = bots
	}
}

func (w *BotWorkers) Next() string {
	w.Lock()
	defer w.Unlock()
	item := w.bots[w.index]
	w.index = (w.index + 1) % len(w.bots)
	return item
}

type Session struct {
	kv  *kv.DB
	key string
}

type kvGet struct {
	key string
	val []byte
}

type kvWrite struct {
	key string
	val []byte
}

func (op *kvGet) Do(ctx context.Context, b kv.Bucket) error {
	data := b.Get([]byte(op.key))
	if len(data) == 0 {
		return ErrNotFound
	}
	op.val = data
	return nil
}

func (op *kvWrite) Do(ctx context.Context, b kv.Bucket) (err error) {
	if err = b.Put([]byte(op.key), op.val); err != nil {
		return err
	}
	return err
}

func NewSession(kv *kv.DB, key string) telegram.SessionStorage {
	return &Session{kv: kv, key: key}
}

func (s *Session) LoadSession(_ context.Context) ([]byte, error) {
	op := kvGet{
		key: s.key,
	}
	err := s.kv.Do(true, &op)

	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	return op.val, nil
}

func (s *Session) StoreSession(_ context.Context, data []byte) error {

	err := s.kv.Do(true, &kvWrite{
		key: s.key,
		val: data,
	})

	return err
}

func key(indexes ...string) string {
	return strings.Join(indexes, ":")
}

func newClient(f *Fs, storage session.Storage, middlewares ...telegram.Middleware) *telegram.Client {

	_clock := tdclock.System

	config := telegram.DeviceConfig{
		DeviceModel:    "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/116.0",
		SystemVersion:  "Win32",
		AppVersion:     "2.1.9 K",
		SystemLangCode: "en",
		LangPack:       "en",
		LangCode:       "webk",
	}

	opts := telegram.Options{
		ReconnectionBackoff: func() backoff.BackOff {
			b := backoff.NewExponentialBackOff()

			b.Multiplier = 1.1
			b.MaxElapsedTime = time.Duration(120) * time.Second
			b.Clock = _clock
			return b
		},
		Device:         config,
		SessionStorage: storage,
		RetryInterval:  5 * time.Second,
		MaxRetries:     5,
		DialTimeout:    10 * time.Second,
		Clock:          _clock,
		Middlewares:    middlewares,
		NoUpdates:      true,
	}

	return telegram.NewClient(f.opt.AppId, f.opt.AppHash, opts)
}

func BotClient(f *Fs, token string) (*telegram.Client, error) {
	storage := NewSession(f.db, key("bot", token))
	middlewares := []telegram.Middleware{floodwait.NewSimpleWaiter()}
	middlewares = append(middlewares, ratelimit.New(rate.Every(time.Millisecond*time.Duration(50)), 20))
	return newClient(f, storage, middlewares...), nil
}

func RunWithAuth(ctx context.Context, client *telegram.Client, token string, f func(ctx context.Context) error) error {
	err := client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}

		if !status.Authorized {
			_, err := client.Auth().Bot(ctx, token)
			if err != nil {
				return err
			}
		}

		return f(ctx)
	})
	return err
}
func GetChannelById(ctx context.Context, client *telegram.Client, channelID int64, userID string) (*tg.InputChannel, error) {

	channel := &tg.InputChannel{}
	inputChannel := &tg.InputChannel{
		ChannelID: channelID,
	}
	channels, err := client.API().ChannelsGetChannels(ctx, []tg.InputChannelClass{inputChannel})

	if err != nil {
		return nil, err
	}

	if len(channels.GetChats()) == 0 {
		return nil, errors.New("no channels found")
	}

	channel = channels.GetChats()[0].(*tg.Channel).AsInput()
	return channel, nil
}

func UploadFile(ctx context.Context, params UploadParams) (int, error) {

	var msgId int

	err := RunWithAuth(ctx, params.Client, params.Token, func(ctx context.Context) error {

		channel, err := GetChannelById(ctx, params.Client, params.ChannelID, params.ChannelUser)

		if err != nil {
			return err
		}

		api := params.Client.API()

		u := uploader.NewUploader(api).WithThreads(16).WithPartSize(512 * 1024)

		upload, err := u.Upload(ctx, uploader.NewUpload(params.Name, params.Body, params.Size))

		if err != nil {
			return err
		}

		document := message.UploadedDocument(upload).Filename(params.Name).ForceFile(true)

		sender := message.NewSender(api)

		target := sender.To(&tg.InputPeerChannel{ChannelID: channel.ChannelID,
			AccessHash: channel.AccessHash})

		res, err := target.Media(ctx, document)

		if err != nil {
			return err
		}

		updates := res.(*tg.Updates)

		var message *tg.Message

		for _, update := range updates.Updates {
			channelMsg, ok := update.(*tg.UpdateNewChannelMessage)
			if ok {
				message = channelMsg.Message.(*tg.Message)
				break
			}
		}

		if message.ID == 0 {
			return errors.New("failed to upload part")
		}
		msgId = message.ID

		return nil
	})

	if err != nil {
		return 0, err
	}

	return msgId, nil
}
