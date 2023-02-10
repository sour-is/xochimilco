package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"git.mills.io/prologic/msgbus"
	"git.mills.io/prologic/msgbus/client"
	"github.com/docopt/docopt-go"
	"github.com/oklog/ulid/v2"
	"go.mills.io/saltyim"

	"github.com/sour-is/xochimilco"
	"github.com/sour-is/xochimilco/cmd/ratchet/xdg"
)

var usage = `Rachet Chat.
Usage:
  ratchet [options] recv
  ratchet [options] (offer|send|close) <them>
  ratchet [options] chat
  ratchet [options] ui

Args:
  <them>             Receiver acct name to use in offer. 

Options:
  --key <key>        Sender private key [default: ` + xdg.Get(xdg.EnvConfigHome, "rachet/$USER.key") + `]
  --state <state>    Session state path [default: ` + xdg.Get(xdg.EnvDataHome, "rachet") + `]
  --msg <msg>        Msg to read in. [default to read Standard Input]
  --msg-file <file>  File to read input from.
  --msg-stdin        Read standard input.
  --post             Send to msgbus
`

type opts struct {
	Offer bool `docopt:"offer"`
	Send  bool `docopt:"send"`
	Recv  bool `docopt:"recv"`
	Close bool `docopt:"close"`
	Chat  bool `docopt:"chat"`
	UI    bool `docopt:"ui"`

	Them string `docopt:"<them>"`

	Key      string `docopt:"--key"`
	Session  string `docopt:"--session"`
	State    string `docopt:"--state"`
	Msg      string `docopt:"--msg"`
	MsgFile  string `docopt:"--msg-file"`
	MsgStdin bool   `docopt:"--msg-stdin"`
	Post     bool   `docopt:"--post"`
}

func main() {
	o, err := docopt.ParseDoc(usage)
	if err != nil {
		log(err)
		os.Exit(2)
	}

	var opts opts
	o.Bind(&opts)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	go func() {
		<-ctx.Done()
		defer cancel() // restore interrupt function
	}()

	if err := run(ctx, opts); err != nil {
		log(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, opts opts) error {
	// log(opts)

	switch {
	case opts.Offer:
		me, key, err := readSaltyIdentity(opts.Key)
		if err != nil {
			return fmt.Errorf("reading keyfile: %w", err)
		}

		sm, close, err := NewSessionManager(opts.State, me, key)
		if err != nil {
			return err
		}
		defer close()

		sess, err := sm.New(opts.Them)
		if err != nil {
			return fmt.Errorf("read session: %w", err)
		}
		log("local session", toULID(sess.LocalUUID).String())
		log("remote session", toULID(sess.RemoteUUID).String())
		msg, err := sess.Offer()
		if err != nil {
			return err
		}

		err = sm.Put(sess)
		if err != nil {
			return err
		}

		fmt.Println(msg)
		if opts.Post {
			addr, err := saltyim.LookupAddr(opts.Them)
			if err != nil {
				return err
			}
			_, err = http.DefaultClient.Post(addr.Endpoint().String(), "text/plain", strings.NewReader(msg))
			if err != nil {
				return err
			}
		}

		return nil

	case opts.Send:
		me, key, err := readSaltyIdentity(opts.Key)
		if err != nil {
			return fmt.Errorf("reading keyfile: %w", err)
		}

		sm, close, err := NewSessionManager(opts.State, me, key)
		if err != nil {
			return err
		}
		defer close()

		input, err := readInput(opts)
		if err != nil {
			return fmt.Errorf("reading input: %w", err)
		}

		sess, err := sm.Get(sm.ByName(opts.Them))
		if err != nil {
			return fmt.Errorf("read session: %w", err)
		}
		log("me:", me, "send:", opts.Them)
		log("local session", toULID(sess.LocalUUID).String())
		log("remote session", toULID(sess.RemoteUUID).String())

		msg, err := sess.Send([]byte(input))
		if err != nil {
			return fmt.Errorf("send: %w", err)
		}
		err = sm.Put(sess)
		if err != nil {
			return err
		}

		fmt.Println(msg)
		if opts.Post {
			addr, err := saltyim.LookupAddr(opts.Them)
			if err != nil {
				return err
			}

			_, err = http.DefaultClient.Post(addr.Endpoint().String(), "text/plain", strings.NewReader(msg))
			if err != nil {
				return err
			}
		}

		return nil

	case opts.Recv:
		me, key, err := readSaltyIdentity(opts.Key)
		if err != nil {
			return fmt.Errorf("reading keyfile: %w", err)
		}

		sm, close, err := NewSessionManager(opts.State, me, key)
		if err != nil {
			return err
		}
		defer close()

		input, err := readInput(opts)
		if err != nil {
			return fmt.Errorf("reading input: %w", err)
		}
		id, msg, err := readMsg(input)
		if err != nil {
			return fmt.Errorf("reading msg: %w", err)
		}
		log("msg session", id.String())

		var sess *Session
		if offer, ok := msg.(interface{ Nick() string }); ok {
			sess, err = sm.New(offer.Nick())
			if err != nil {
				return fmt.Errorf("get session: %w", err)
			}
		} else {
			sess, err = sm.Get(id)
			if err != nil {
				return fmt.Errorf("get session: %w", err)
			}
		}
		log("local session", toULID(sess.LocalUUID).String())
		log("remote session", toULID(sess.RemoteUUID).String())

		isEstablished, isClosed, plaintext, err := sess.ReceiveMsg(msg)
		if err != nil {
			return fmt.Errorf("session receive: %w", err)
		}
		log("(updated) remote session", toULID(sess.RemoteUUID).String())

		err = sm.Put(sess)
		if err != nil {
			return err
		}

		switch {
		case isClosed:
			log("GOT: closing session...")
			return sm.Delete(sess)
		case isEstablished:
			log("GOT: session established with ", sess.Name, "...")
			if len(plaintext) > 0 {
				fmt.Println(string(plaintext))
				if opts.Post {
					addr, err := saltyim.LookupAddr(opts.Them)
					if err != nil {
						return err
					}

					_, err = http.DefaultClient.Post(addr.Endpoint().String(), "text/plain", bytes.NewReader(plaintext))
					if err != nil {
						return err
					}
				}
			}

		default:
			log("GOT: ", sess.Name, ">", string(plaintext))
		}

		return nil

	case opts.Close:
		me, key, err := readSaltyIdentity(opts.Key)
		if err != nil {
			return fmt.Errorf("reading keyfile: %w", err)
		}

		sm, close, err := NewSessionManager(opts.State, me, key)
		if err != nil {
			return err
		}
		defer close()

		sess, err := sm.Get(sm.ByName(opts.Them))
		if err != nil {
			return fmt.Errorf("read session: %w", err)
		}

		msg, err := sess.Close()
		if err != nil {
			return fmt.Errorf("session close: %w", err)
		}

		err = sm.Delete(sess)
		if err != nil {
			return err
		}

		fmt.Println(msg)
		if opts.Post {
			addr, err := saltyim.LookupAddr(opts.Them)
			if err != nil {
				return err
			}

			_, err = http.DefaultClient.Post(addr.Endpoint().String(), "text/plain", strings.NewReader(msg))
			if err != nil {
				return err
			}
		}
		return nil

	case opts.Chat:
		var state = struct {
			offers map[string]xochimilco.Msg
			chats  map[ulid.ULID]*Session
		}{
			offers: make(map[string]xochimilco.Msg),
			chats:  make(map[ulid.ULID]*Session),
		}

		me, key, err := readSaltyIdentity(opts.Key)
		if err != nil {
			return fmt.Errorf("reading keyfile: %w", err)
		}

		addr, err := saltyim.LookupAddr(me)
		if err != nil {
			return fmt.Errorf("lookup addr: %w", err)
		}

		sm, close, err := NewSessionManager(opts.State, me, key)
		if err != nil {
			return err
		}
		defer close()

		uri, inbox := saltyim.SplitInbox(addr.Endpoint().String())
		bus := client.NewClient(uri, nil)

		log("listen to", uri, inbox)

		handleFn := func(in *msgbus.Message) error {
			input := string(in.Payload)
			if !(strings.HasPrefix(input, "!RAT!") && strings.HasSuffix(input, "!CHT!")) {
				return nil
			}

			// log(input)
			id, msg, err := readMsg(input)
			if err != nil {
				return fmt.Errorf("reading msg: %w", err)
			}
			// log("msg session", id.String())

			var sess *Session
			if offer, ok := msg.(interface{ Nick() string }); ok {
				log("got offer from: ", offer.Nick())
				state.offers[offer.Nick()] = msg
				return nil
			} else {
				sess, err = sm.Get(id)
				if err != nil {
					return nil // no session. ignore.
				}
				state.chats[toULID(sess.LocalUUID)] = sess
			}
			// log("local session", toULID(sess.LocalUUID).String())
			// log("remote session", toULID(sess.RemoteUUID).String())

			isEstablished, isClosed, plaintext, err := sess.ReceiveMsg(msg)
			if err != nil {
				return fmt.Errorf("session receive: %w", err)
			}
			// log("(updated) remote session", toULID(sess.RemoteUUID).String())

			err = sm.Put(sess)
			if err != nil {
				return err
			}

			switch {
			case isClosed:
				log("GOT: closing session...")
				return sm.Delete(sess)
			case isEstablished:
				log("GOT: session established with ", sess.Name, "...")
				if len(plaintext) > 0 {
					// fmt.Println(string(plaintext))
					if opts.Post {
						addr, err := saltyim.LookupAddr(sess.Name)
						if err != nil {
							return err
						}

						_, err = http.DefaultClient.Post(addr.Endpoint().String(), "text/plain", bytes.NewReader(plaintext))
						if err != nil {
							return err
						}
					}
				}
			default:
				log("GOT: ", sess.Name, ">", string(plaintext))
			}

			return nil
		}

		s := bus.Subscribe(inbox, 0, handleFn)
		return s.Run(ctx)

	case opts.UI:

		return nil

	default:
		log(usage)
	}

	return nil
}

func log(a ...any) {
//	fmt.Fprint(os.Stderr, "\033[1A\r\033[2K")
	fmt.Fprintln(os.Stderr, a...)
	fmt.Fprint(os.Stderr, ">")
}
