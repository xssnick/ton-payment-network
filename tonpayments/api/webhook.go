package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/rs/zerolog/log"
	"net/http"
	"time"
)

const WebhooksTaskPool = "wp"

type WebhookRequest struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	EventTime time.Time       `json:"event_time"`
}

type WebhookResponse struct {
	Success bool `json:"success"`
}

func (s *Server) startWebhooksSender() error {
	tick := time.Tick(1 * time.Second)

	for {
		task, err := s.queue.AcquireTask(context.Background(), WebhooksTaskPool)
		if err != nil {
			log.Error().Err(err).Msg("failed to acquire webhook task from db")
			time.Sleep(3 * time.Second)
			continue
		}

		if task == nil {
			select {
			case <-s.webhookSignal:
			case <-tick:
			}
			continue
		}

		// run each task in own routine, to not block other's execution
		go func() {
			err = func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				buf := new(bytes.Buffer)
				if err = json.NewEncoder(buf).Encode(WebhookRequest{
					ID:        task.ID,
					Type:      task.Type,
					Data:      task.Data,
					EventTime: task.CreatedAt,
				}); err != nil {
					return fmt.Errorf("failed to serialize body: %w", err)
				}

				req, err := http.NewRequestWithContext(ctx, "POST", s.webhook, nil)
				if err != nil {
					return fmt.Errorf("failed to build request: %w", err)
				}

				// TODO: sign hmac

				resp, err := s.sender.Do(req)
				if err != nil {
					return fmt.Errorf("failed to send webhook: %w", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("webhook response status is: %d %s", resp.StatusCode, resp.Status)
				}

				var ok WebhookResponse
				if err = json.NewDecoder(resp.Body).Decode(&ok); err != nil {
					return fmt.Errorf("bad webhook response: %w", err)
				}

				if !ok.Success {
					return fmt.Errorf("webhook response is not success")
				}

				return nil
			}()
			if err != nil {
				log.Warn().Err(err).Str("type", task.Type).Str("id", task.ID).Msg("task execute err, will be retried")

				// random wait to not lock both sides in same time
				retryAfter := time.Now().Add(300 * time.Millisecond)
				if err = s.queue.RetryTask(context.Background(), task, err.Error(), retryAfter); err != nil {
					log.Error().Err(err).Str("id", task.ID).Msg("failed to set failure for task in db")
				}
				return
			}

			if err = s.queue.CompleteTask(context.Background(), WebhooksTaskPool, task); err != nil {
				log.Error().Err(err).Str("id", task.ID).Msg("failed to set complete for task in db")
			}

			s.touchWebhook()
		}()
	}
}

// touchWebhook - forces worker to check db tasks
func (s *Server) touchWebhook() {
	select {
	case s.webhookSignal <- true:
		// ask queue to take new task without waiting
	default:
	}
}
