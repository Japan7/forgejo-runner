package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

type Message struct {
	Version      int    //
	Type         int    // message type, 1 register 2 error
	RunnerUUID   string // runner uuid
	BuildUUID    string // build uuid
	ErrCode      int    // error code
	ErrContent   string // errors message
	EventName    string
	EventPayload string
	JobID        string // only run the special job, empty means run all the jobs
}

const (
	MsgTypeRegister     = iota + 1 // register
	MsgTypeError                   // error
	MsgTypeRequestBuild            // request build task
	MsgTypeIdle                    // no task
	MsgTypeBuildResult             // build result
)

func handleVersion1(conn *websocket.Conn, sigs chan os.Signal, message []byte, msg *Message) error {
	switch msg.Type {
	case MsgTypeRegister:
		log.Info().Msgf("received registered success: %s", message)
		return conn.WriteJSON(&Message{
			Version:    1,
			Type:       MsgTypeRequestBuild,
			RunnerUUID: msg.RunnerUUID,
		})
	case MsgTypeError:
		log.Info().Msgf("received error msessage: %s", message)
		return conn.WriteJSON(&Message{
			Version:    1,
			Type:       MsgTypeRequestBuild,
			RunnerUUID: msg.RunnerUUID,
		})
	case MsgTypeIdle:
		log.Info().Msgf("received no task")
		return conn.WriteJSON(&Message{
			Version:    1,
			Type:       MsgTypeRequestBuild,
			RunnerUUID: msg.RunnerUUID,
		})
	case MsgTypeRequestBuild:
		switch msg.EventName {
		case "push":
			input := Input{
				forgeInstance:   "github.com",
				reuseContainers: true,
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			defer cancel()

			done := make(chan error)
			go func(chan error) {
				done <- runTask(ctx, &input, "")
			}(done)

			c := time.NewTicker(time.Second)
			defer c.Stop()

			for {
				select {
				case <-sigs:
					cancel()
					log.Info().Msgf("cancel task")
					return nil
				case err := <-done:
					if err != nil {
						log.Error().Msgf("runTask failed: %v", err)
						return conn.WriteJSON(&Message{
							Version:    1,
							Type:       MsgTypeBuildResult,
							RunnerUUID: msg.RunnerUUID,
							BuildUUID:  msg.BuildUUID,
							ErrCode:    1,
							ErrContent: err.Error(),
						})
					}
					log.Error().Msgf("runTask success")
					return conn.WriteJSON(&Message{
						Version:    1,
						Type:       MsgTypeBuildResult,
						RunnerUUID: msg.RunnerUUID,
						BuildUUID:  msg.BuildUUID,
					})
				case <-c.C:
				}
			}
		default:
			return fmt.Errorf("unknow event %s with payload %s", msg.EventName, msg.EventPayload)
		}
	default:
		return fmt.Errorf("received a message with an unsupported type: %#v", msg)
	}
}

// TODO: handle the message
func handleMessage(conn *websocket.Conn, sigs chan os.Signal, message []byte) error {
	var msg Message
	if err := json.Unmarshal(message, &msg); err != nil {
		return fmt.Errorf("unmarshal received message faild: %v", err)
	}

	switch msg.Version {
	case 1:
		return handleVersion1(conn, sigs, message, &msg)
	default:
		return fmt.Errorf("recevied a message with an unsupported version, consider upgrade your runner")
	}
}

func runDaemon(ctx context.Context, input *Input) func(cmd *cobra.Command, args []string) error {
	log.Info().Msgf("Starting runner daemon")

	return func(cmd *cobra.Command, args []string) error {
		var conn *websocket.Conn
		var err error
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var failedCnt int
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

		for {
			select {
			case <-sigs:
				log.Info().Msgf("cancel task")
				return nil

			case <-ctx.Done():
				if conn != nil {
					err = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
					if err != nil {
						log.Error().Msgf("write close: %v", err)
					}
				}
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil
				}
				return ctx.Err()
			case <-ticker.C:
				if conn == nil {
					log.Trace().Msgf("trying connect %v", "ws://localhost:3000/api/actions")
					conn, _, err = websocket.DefaultDialer.DialContext(ctx, "ws://localhost:3000/api/actions", nil)
					if err != nil {
						log.Error().Msgf("dial: %v", err)
						break
					}

					// register the client
					msg := Message{
						Version:    1,
						Type:       MsgTypeRegister,
						RunnerUUID: "111111",
					}
					bs, err := json.Marshal(&msg)
					if err != nil {
						log.Error().Msgf("Marshal: %v", err)
						break
					}

					if err = conn.WriteMessage(websocket.TextMessage, bs); err != nil {
						log.Error().Msgf("register failed: %v", err)
						conn.Close()
						conn = nil
						break
					}
				}

				const timeout = time.Second * 10

				for {
					select {
					case <-sigs:
						log.Info().Msgf("cancel task")
						return nil
					default:
					}

					conn.SetReadDeadline(time.Now().Add(timeout))
					conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(timeout)); return nil })

					_, message, err := conn.ReadMessage()
					if err != nil {
						if websocket.IsCloseError(err, websocket.CloseAbnormalClosure) ||
							websocket.IsCloseError(err, websocket.CloseNormalClosure) {
							log.Trace().Msgf("closed from remote")
							conn.Close()
							conn = nil
						} else if !strings.Contains(err.Error(), "i/o timeout") {
							log.Error().Msgf("read message failed: %#v", err)
						}
						failedCnt++
						if failedCnt > 60 {
							if conn != nil {
								conn.Close()
								conn = nil
							}
							failedCnt = 0
						}
						return nil
					}

					if err := handleMessage(conn, sigs, message); err != nil {
					}
				}
			}
		}
	}
}