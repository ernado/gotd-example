package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/go-faster/errors"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/contrib/pebble"
	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/updates"
	updhook "github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
	"golang.org/x/time/rate"
	lj "gopkg.in/natefinch/lumberjack.v2"
)

// terminalAuth implements auth.UserAuthenticator prompting the terminal for
// input.
type terminalAuth struct {
	phone string
}

func (terminalAuth) SignUp(ctx context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("not implemented")
}

func (terminalAuth) AcceptTermsOfService(ctx context.Context, tos tg.HelpTermsOfService) error {
	return &auth.SignUpRequired{TermsOfService: tos}
}

func (terminalAuth) Code(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
	fmt.Print("Enter code: ")
	code, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(code), nil
}

func (a terminalAuth) Phone(_ context.Context) (string, error) {
	return a.phone, nil
}

func (terminalAuth) Password(_ context.Context) (string, error) {
	fmt.Print("Enter 2FA password: ")
	bytePwd, err := term.ReadPassword(syscall.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(bytePwd)), nil
}

func sessionFolder(phone string) string {
	var out []rune
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			out = append(out, r)
		}
	}
	return "phone-" + string(out)
}

func run(ctx context.Context) (rerr error) {
	var arg struct {
		FillPeerStorage bool
	}
	flag.BoolVar(&arg.FillPeerStorage, "fill-peer-storage", false, "fill peer storage")
	flag.Parse()

	// Using ".env" file to load environment variables.
	err := godotenv.Load()
	if err != nil {
		return errors.Wrap(err, "load env")
	}

	// TG_PHONE is phone number in international format.
	// Like +4123456789.
	phone := os.Getenv("TG_PHONE")
	if phone == "" {
		return errors.New("no phone")
	}
	// APP_HASH, APP_ID is from https://my.telegram.org/.
	appID, err := strconv.Atoi(os.Getenv("APP_ID"))
	if err != nil {
		return errors.Wrap(err, " parse app id")
	}
	appHash := os.Getenv("APP_HASH")
	if appHash == "" {
		return errors.New("no app hash")
	}

	// Setting up session storage.
	// This is needed to reuse session and not login every time.
	sessionDir := filepath.Join("session", sessionFolder(phone))
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return err
	}
	logFilePath := filepath.Join(sessionDir, "log.jsonl")

	fmt.Printf("Storing session in %s, logs in %s\n", sessionDir, logFilePath)

	// Setting up logging to file with rotation.
	//
	// Log to file, so we don't interfere with prompts and messages to user.
	logWriter := zapcore.AddSync(&lj.Logger{
		Filename:   logFilePath,
		MaxBackups: 3,
		MaxSize:    1, // megabytes
		MaxAge:     7, // days
	})
	logCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		logWriter,
		zap.DebugLevel,
	)
	lg := zap.New(logCore)
	defer func() { _ = lg.Sync() }()

	// So, we are storing session information in current directory, under subdirectory "session/phone_hash"
	sessionStorage := &telegram.FileSessionStorage{
		Path: filepath.Join(sessionDir, "session.json"),
	}
	// Peer storage, for resolve caching and short updates handling.
	db, err := pebbledb.Open(filepath.Join(sessionDir, "peers.pebble.db"), &pebbledb.Options{})
	if err != nil {
		return errors.Wrap(err, "create pebble storage")
	}
	defer func() {
		// Ensuring that db is closed correctly.
		closeErr := db.Close()
		if closeErr == nil {
			return
		}
		if rerr == nil {
			multierr.AppendInto(&rerr, closeErr)
		} else {
			rerr = closeErr
		}
	}()
	peerDB := pebble.NewPeerStorage(db)
	lg.Info("Storage", zap.String("path", sessionDir))

	// Setting up client.
	//
	// Dispatcher is used to register handlers for events.
	dispatcher := tg.NewUpdateDispatcher()
	// Setting up update handler that will fill peer storage before
	// calling dispatcher handlers.
	//
	// Wrapping dispatcher (previous update handler) via UpdateHook.
	peerDBHandler := storage.UpdateHook(dispatcher, peerDB)

	// Setting up updates recovery handler that will fetch missing updates
	// after restart or reconnect.
	//
	// First, we need to store state of updates handler. If no storage,
	// only reconnects can be handled.
	//
	// The BoltState is state storage implementation based on bbolt.
	stateDB, err := bolt.Open(filepath.Join(sessionDir, "updates.state.bbolt"), fs.ModePerm, bolt.DefaultOptions)
	if err != nil {
		return errors.Wrap(err, "state database")
	}
	defer func() {
		// Ensuring that state database is closed correctly.
		closeErr := stateDB.Close()
		if closeErr == nil {
			return
		}
		if rerr == nil {
			multierr.AppendInto(&rerr, closeErr)
		} else {
			rerr = closeErr
		}
	}()
	updatesHandler := updates.New(updates.Config{
		// Wrapping previous handler.
		Handler: storage.UpdateHook(peerDBHandler, peerDB),
		Storage: NewBoltState(stateDB),
		Logger:  lg.Named("gaps"),
	})

	// Handler of FLOOD_WAIT that will automatically retry request.
	waiter := floodwait.NewWaiter().WithCallback(func(ctx context.Context, wait floodwait.FloodWait) {
		// Notifying about flood wait.
		lg.Warn("Flood wait", zap.Duration("wait", wait.Duration))
		fmt.Println("Got FLOOD_WAIT. Will retry after", wait.Duration)
	})

	// Filling client options.
	options := telegram.Options{
		Logger:         lg,             // Passing logger for observability.
		SessionStorage: sessionStorage, // Setting up session sessionStorage to store auth data.
		UpdateHandler:  updatesHandler, // Setting up handler for updates from server.
		Middlewares: []telegram.Middleware{
			// Setting up FLOOD_WAIT handler to automatically wait and retry request.
			//
			// NB: If disabled, you will get FLOOD_WAIT errors and will need to retry manually.
			waiter,
			// Setting up general rate limits to less likely get flood wait errors.
			ratelimit.New(rate.Every(time.Millisecond*100), 5),

			// NB: This is critical for updates handler to work.
			updhook.UpdateHook(updatesHandler.Handle),
		},
	}
	client := telegram.NewClient(appID, appHash, options)
	api := client.API()

	// You can also use peer resolver cache to resolve peers.
	_ = storage.NewResolverCache(peer.Plain(api), peerDB)

	// Registering handler for new private messages.
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}
		if msg.Out {
			// Outgoing message.
			return nil
		}

		// Use PeerID to find peer because *Short updates does not contain any entities, so it necessary to
		// store some entities.
		//
		// Storage can be filled using PeerCollector (i.e. fetching all dialogs first).
		p, err := storage.FindPeer(ctx, peerDB, msg.GetPeerID())
		if err != nil {
			return err
		}

		fmt.Printf("%s: %s\n", p, msg.Message)

		// Marking message as read.
		if _, err := api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
			Peer:  p.AsInputPeer(),
			MaxID: msg.ID,
		}); err != nil {
			return errors.Wrap(err, "read history")
		}

		return nil
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}
		if msg.Out {
			// Outgoing message.
			return nil
		}

		// Use PeerID to find peer because *Short updates does not contain any entities, so it necessary to
		// store some entities.
		//
		// Storage can be filled using PeerCollector (i.e. fetching all dialogs first).
		p, err := storage.FindPeer(ctx, peerDB, msg.GetPeerID())
		if err != nil {
			lg.Error("Find peer", zap.Error(err))
			return errors.Wrap(err, "find peer")
		}

		fmt.Printf("%s: %s\n", p, msg.Message)

		channel, ok := p.AsInputChannel()
		if !ok {
			return errors.New("not a channel")
		}
		if _, err := api.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: channel,
			MaxID:   msg.ID,
		}); err != nil {
			return errors.Wrap(err, "read history")
		}

		return nil
	})

	// Authentication flow handles authentication process, like prompting for code and 2FA password.
	authFlow := auth.NewFlow(terminalAuth{phone: phone}, auth.SendCodeOptions{})

	handler := func(ctx context.Context) error {
		if self, err := client.Self(ctx); err != nil || self.Bot {
			// Starting authentication flow.
			fmt.Println("Not logged in: starting auth")
			lg.Info("Starting authentication flow")
			if err := authFlow.Run(ctx, client.Auth()); err != nil {
				return errors.Wrap(err, "auth")
			}
		} else {
			fmt.Println("Already logged in")
			lg.Info("Already authenticated")
		}

		// Getting info about current user.
		self, err := client.Self(ctx)
		if err != nil {
			return errors.Wrap(err, "call self")
		}

		ready := make(chan struct{})
		wg, ctx := errgroup.WithContext(ctx)
		wg.Go(func() error {
			// Start update manager.
			//
			// NB: this is critical for updates handler to work.
			return updatesHandler.Run(ctx, api, self.ID, updates.AuthOptions{
				OnStart: func(ctx context.Context) {
					close(ready)
					lg.Info("Updates handler started")
				},
			})
		})
		wg.Go(func() error {
			select {
			case <-ready:
			case <-ctx.Done():
				return ctx.Err()
			}

			name := self.FirstName
			if self.Username != "" {
				// Username is optional.
				name = fmt.Sprintf("%s (@%s)", name, self.Username)
			}
			fmt.Println("Current user:", name)

			lg.Info("Login",
				zap.String("first_name", self.FirstName),
				zap.String("last_name", self.LastName),
				zap.String("username", self.Username),
				zap.Int64("id", self.ID),
			)

			if arg.FillPeerStorage {
				fmt.Println("Filling peer storage from dialogs to cache entities")
				collector := storage.CollectPeers(peerDB)
				if err := collector.Dialogs(ctx, query.GetDialogs(api).Iter()); err != nil {
					return errors.Wrap(err, "collect peers")
				}
				fmt.Println("Filled")
			}

			return nil
		})

		// Waiting until context is done.
		fmt.Println("Listening for updates. Interrupt (Ctrl+C) to stop.")
		return wg.Wait()
	}

	if err := waiter.Run(ctx, func(ctx context.Context) error {
		// Client should be started after waiter.
		return client.Run(ctx, handler)
	}); err != nil {
		return errors.Wrap(err, "run client")
	}

	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := run(ctx); err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() == context.Canceled {
			fmt.Println("\rClosed")
			os.Exit(0)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Error: %+v\n", err)
		os.Exit(1)
	} else {
		fmt.Println("Done")
		os.Exit(0)
	}
}
