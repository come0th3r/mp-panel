package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (a *API) consoleWS(w http.ResponseWriter, r *http.Request) {
	s, ok := a.getServer(w, r)
	if !ok {
		return
	}
	origin := r.Header.Get("Origin")
	if origin == "" || !a.sameOrigin(r, origin) {
		fail(w, 403, "Недопустимый источник WebSocket")
		return
	}
	if e := a.readyNode(r.Context(), s.NodeID); e != nil {
		fail(w, 409, e.Error())
		return
	}
	up := upgrader
	up.CheckOrigin = func(req *http.Request) bool { return a.sameOrigin(req, req.Header.Get("Origin")) }
	conn, e := up.Upgrade(w, r, nil)
	if e != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(8192)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	errorsOut := make(chan string, 8)
	go func() {
		defer cancel()
		for {
			var message struct {
				Command string `json:"command"`
			}
			if e := conn.ReadJSON(&message); e != nil {
				return
			}
			// Re-read both the role and assignment after receiving each message;
			// an open socket must not preserve a grant that has been revoked.
			u, _, e := a.session(r)
			if e != nil {
				return
			}
			access, e := a.serverAccess(ctx, u, s.ID)
			if e != nil || !access {
				return
			}
			commandRequest := r.WithContext(context.WithValue(ctx, userKey, u))
			if !allowed(u, "operate") {
				select {
				case errorsOut <- "Нет права отправлять команды":
				default:
				}
				continue
			}
			if message.Command == "" || len(message.Command) > 4096 || strings.ContainsAny(message.Command, "\r\n\x00") {
				select {
				case errorsOut <- "Введите одну команду длиной до 4096 символов":
				default:
				}
				continue
			}
			_, e = a.syncCommand(commandRequest, s, "console", message, false)
			if e != nil {
				select {
				case errorsOut <- e.Error():
				default:
				}
			} else {
				a.audit(ctx, u, "console.write", s.ID, "Команда отправлена")
			}
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	previous := ""
	for {
		u, _, e := a.session(r)
		if e != nil {
			return
		}
		access, e := a.serverAccess(ctx, u, s.ID)
		if e != nil || !access {
			return
		}
		logRequest := r.WithContext(context.WithValue(ctx, userKey, u))
		out, e := a.syncCommand(logRequest, s, "logs", map[string]string{}, false)
		// Log reads can wait for an agent. Check again before exposing output.
		currentUser, _, sessionErr := a.session(r)
		access, accessErr := a.serverAccess(ctx, currentUser, s.ID)
		if sessionErr != nil || accessErr != nil || !access {
			return
		}
		if e != nil {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteJSON(map[string]string{"error": e.Error()}) != nil {
				return
			}
		} else if string(out) != previous {
			var lines any
			if json.Unmarshal(out, &lines) == nil {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if conn.WriteJSON(lines) != nil {
					return
				}
				previous = string(out)
			}
		}
		select {
		case <-ctx.Done():
			return
		case err := <-errorsOut:
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteJSON(map[string]string{"error": err}) != nil {
				return
			}
		case <-ticker.C:
		}
	}
}
