package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/come0th3r/mp-panel/internal/model"
	"github.com/come0th3r/mp-panel/internal/security"
)

func (a *API) session(r *http.Request) (model.User, string, error) {
	var u model.User
	var csrf string
	cookie, e := r.Cookie("mp_session")
	if e != nil {
		return u, "", e
	}
	e = a.Store.Pool.QueryRow(r.Context(), `SELECT u.id,u.username,u.role,u.totp_secret IS NOT NULL,s.csrf_token FROM mp_sessions s JOIN mp_users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now()`, security.Hash(cookie.Value)).Scan(&u.ID, &u.Username, &u.Role, &u.TOTPEnabled, &csrf)
	return u, csrf, e
}
func (a *API) authStatus(w http.ResponseWriter, r *http.Request) {
	var count int
	if e := a.Store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM mp_users`).Scan(&count); e != nil {
		fail(w, 503, "База данных недоступна")
		return
	}
	out := map[string]any{"initialized": count > 0, "authenticated": false, "setup_token_required": count == 0}
	if u, csrf, e := a.session(r); e == nil {
		out["authenticated"] = true
		out["user"] = u
		out["csrf_token"] = csrf
	}
	write(w, 200, out)
}
func (a *API) newSession(w http.ResponseWriter, r *http.Request, u model.User) {
	token := security.Token()
	csrf := security.Token()
	if _, e := a.Store.Pool.Exec(r.Context(), `INSERT INTO mp_sessions(token_hash,user_id,csrf_token,expires_at) VALUES($1,$2,$3,$4)`, security.Hash(token), u.ID, csrf, time.Now().Add(24*time.Hour)); e != nil {
		fail(w, 500, "Не удалось создать сессию")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "mp_session", Value: token, Path: "/", HttpOnly: true, Secure: a.Config.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	write(w, 200, map[string]any{"initialized": true, "authenticated": true, "user": u, "csrf_token": csrf})
}
func validAccount(username, password string) bool {
	return len(username) >= 3 && len(username) <= 80 && strings.TrimSpace(username) == username && len(password) >= 12 && len(password) <= 1024 && !strings.ContainsAny(username, "\r\n\x00")
}
func (a *API) SetupToken() (string, error) {
	if token := os.Getenv("MP_SETUP_TOKEN"); token != "" {
		return token, nil
	}
	path := filepath.Join(a.Config.DataDir, "keys", "setup.token")
	if b, e := os.ReadFile(path); e == nil {
		return strings.TrimSpace(string(b)), nil
	} else if !os.IsNotExist(e) {
		return "", e
	}
	token := security.Token()
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(e) {
		b, e := os.ReadFile(path)
		return strings.TrimSpace(string(b)), e
	}
	if e != nil {
		return "", e
	}
	defer f.Close()
	_, e = f.WriteString(token + "\n")
	return token, e
}
func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	if a.limited(r) {
		fail(w, 429, "Слишком много попыток. Попробуйте через минуту")
		return
	}
	var p struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		SetupToken string `json:"setup_token"`
	}
	if !decode(w, r, &p) {
		return
	}
	if !validAccount(p.Username, p.Password) {
		fail(w, 400, "Имя: 3–80 символов. Пароль: не менее 12 символов")
		return
	}
	want, e := a.SetupToken()
	if e != nil || subtle.ConstantTimeCompare([]byte(want), []byte(p.SetupToken)) != 1 {
		fail(w, 403, "Неверный ключ первоначальной настройки")
		return
	}
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		fail(w, 503, "База данных недоступна")
		return
	}
	defer tx.Rollback(r.Context())
	if _, e = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(628120)`); e != nil {
		fail(w, 500, "Не удалось подготовить учетную запись")
		return
	}
	var count int
	if e = tx.QueryRow(r.Context(), `SELECT count(*) FROM mp_users`).Scan(&count); e != nil {
		fail(w, 500, "Ошибка базы данных")
		return
	}
	if count > 0 {
		fail(w, 409, "Владелец панели уже создан")
		return
	}
	u := model.User{ID: security.ID(), Username: p.Username, Role: "owner"}
	if _, e = tx.Exec(r.Context(), `INSERT INTO mp_users(id,username,password_hash,role) VALUES($1,$2,$3,$4)`, u.ID, u.Username, security.PasswordHash(p.Password), u.Role); e != nil {
		fail(w, 500, "Не удалось создать владельца")
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		fail(w, 500, "Не удалось сохранить владельца")
		return
	}
	a.audit(r.Context(), u, "auth.setup", u.ID, "")
	a.newSession(w, r, u)
}
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if a.limited(r) {
		fail(w, 429, "Слишком много попыток входа. Попробуйте через минуту")
		return
	}
	var p struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &p) {
		return
	}
	if len(p.Password) > 1024 {
		fail(w, 401, "Неверное имя или пароль")
		return
	}
	var u model.User
	var hash string
	var secret []byte
	e := a.Store.Pool.QueryRow(r.Context(), `SELECT id,username,role,password_hash,totp_secret FROM mp_users WHERE username=$1`, p.Username).Scan(&u.ID, &u.Username, &u.Role, &hash, &secret)
	if e != nil || !security.PasswordOK(p.Password, hash) {
		fail(w, 401, "Неверное имя или пароль")
		return
	}
	u.TOTPEnabled = secret != nil
	if secret != nil {
		if p.Code == "" {
			write(w, 401, map[string]string{"error": "Введите код приложения-аутентификатора или код восстановления", "code": "totp_required"})
			return
		}
		if e = a.consumeFactor(r, u.ID, p.Code); e != nil {
			fail(w, 401, "Неверный или уже использованный код")
			return
		}
	}
	a.audit(r.Context(), u, "auth.login", u.ID, "")
	a.newSession(w, r, u)
}
func (a *API) consumeFactor(r *http.Request, userID, code string) error {
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	var encrypted, raw []byte
	var last int64
	e = tx.QueryRow(r.Context(), `SELECT totp_secret,totp_last,recovery_codes FROM mp_users WHERE id=$1 FOR UPDATE`, userID).Scan(&encrypted, &last, &raw)
	if e != nil {
		return e
	}
	if encrypted == nil {
		return fmt.Errorf("2FA not enabled")
	}
	secret, e := a.Vault.Decrypt(encrypted)
	if e != nil {
		return e
	}
	counter, e := security.TOTPCounter(string(secret), strings.TrimSpace(code), last)
	if e == nil {
		_, e = tx.Exec(r.Context(), `UPDATE mp_users SET totp_last=$2 WHERE id=$1`, userID, counter)
	} else {
		var codes []string
		if json.Unmarshal(raw, &codes) != nil {
			return e
		}
		target := security.Hash(strings.TrimSpace(code))
		found := -1
		for i, c := range codes {
			if subtle.ConstantTimeCompare([]byte(target), []byte(c)) == 1 {
				found = i
			}
		}
		if found < 0 {
			return e
		}
		codes = append(codes[:found], codes[found+1:]...)
		encoded, _ := json.Marshal(codes)
		_, e = tx.Exec(r.Context(), `UPDATE mp_users SET recovery_codes=$2 WHERE id=$1`, userID, encoded)
	}
	if e != nil {
		return e
	}
	return tx.Commit(r.Context())
}
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("mp_session"); e == nil {
		_, _ = a.Store.Pool.Exec(r.Context(), `DELETE FROM mp_sessions WHERE token_hash=$1`, security.Hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "mp_session", Value: "", Path: "/", HttpOnly: true, Secure: a.Config.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	write(w, 200, map[string]bool{"ok": true})
}
func (a *API) totpSetup(w http.ResponseWriter, r *http.Request) {
	u := actor(r)
	if u.TOTPEnabled {
		fail(w, 409, "2FA уже включена")
		return
	}
	secret := security.NewTOTPSecret()
	encrypted, e := a.Vault.Encrypt([]byte(secret))
	if e != nil {
		fail(w, 500, "Не удалось подготовить 2FA")
		return
	}
	if _, e = a.Store.Pool.Exec(r.Context(), `UPDATE mp_users SET totp_pending=$2 WHERE id=$1 AND totp_secret IS NULL`, u.ID, encrypted); e != nil {
		fail(w, 500, "Не удалось сохранить 2FA")
		return
	}
	uri := "otpauth://totp/" + url.PathEscape("MP:"+u.Username) + "?secret=" + secret + "&issuer=MP&algorithm=SHA1&digits=6&period=30"
	write(w, 200, map[string]string{"secret": secret, "uri": uri})
}
func (a *API) totpEnable(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &p) {
		return
	}
	u := actor(r)
	tx, e := a.Store.Pool.Begin(r.Context())
	if e != nil {
		fail(w, 500, "Ошибка базы данных")
		return
	}
	defer tx.Rollback(r.Context())
	var pending, existing []byte
	if e = tx.QueryRow(r.Context(), `SELECT totp_pending,totp_secret FROM mp_users WHERE id=$1 FOR UPDATE`, u.ID).Scan(&pending, &existing); e != nil || pending == nil || existing != nil {
		fail(w, 409, "Сначала начните настройку 2FA")
		return
	}
	plain, e := a.Vault.Decrypt(pending)
	if e != nil {
		fail(w, 500, "Не удалось прочитать секрет 2FA")
		return
	}
	counter, e := security.TOTPCounter(string(plain), p.Code, -1)
	if e != nil {
		fail(w, 400, "Неверный код аутентификатора")
		return
	}
	codes := make([]string, 10)
	hashes := make([]string, 10)
	for i := range codes {
		codes[i] = security.Token()[:20]
		hashes[i] = security.Hash(codes[i])
	}
	raw, _ := json.Marshal(hashes)
	if _, e = tx.Exec(r.Context(), `UPDATE mp_users SET totp_secret=totp_pending,totp_pending=NULL,totp_last=$2,recovery_codes=$3 WHERE id=$1`, u.ID, counter, raw); e != nil {
		fail(w, 500, "Не удалось включить 2FA")
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		fail(w, 500, "Не удалось сохранить 2FA")
		return
	}
	a.audit(r.Context(), u, "auth.totp.enable", u.ID, "")
	write(w, 200, map[string]any{"recovery_codes": codes})
}
func (a *API) totpDisable(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &p) {
		return
	}
	if a.limited(r) {
		fail(w, 429, "Слишком много попыток")
		return
	}
	u := actor(r)
	var hash string
	e := a.Store.Pool.QueryRow(r.Context(), `SELECT password_hash FROM mp_users WHERE id=$1`, u.ID).Scan(&hash)
	if e != nil || !security.PasswordOK(p.Password, hash) || a.consumeFactor(r, u.ID, p.Code) != nil {
		fail(w, 403, "Неверный пароль или код")
		return
	}
	if _, e = a.Store.Pool.Exec(r.Context(), `UPDATE mp_users SET totp_secret=NULL,totp_pending=NULL,totp_last=-1,recovery_codes='[]' WHERE id=$1`, u.ID); e != nil {
		fail(w, 500, "Не удалось отключить 2FA")
		return
	}
	a.audit(r.Context(), u, "auth.totp.disable", u.ID, "")
	write(w, 200, map[string]bool{"ok": true})
}
func (a *API) users(w http.ResponseWriter, r *http.Request) {
	rows, e := a.Store.Pool.Query(r.Context(), `SELECT id,username,role,totp_secret IS NOT NULL FROM mp_users ORDER BY created_at`)
	if e != nil {
		fail(w, 500, "Не удалось получить пользователей")
		return
	}
	defer rows.Close()
	out := []model.User{}
	for rows.Next() {
		var u model.User
		if rows.Scan(&u.ID, &u.Username, &u.Role, &u.TOTPEnabled) != nil {
			fail(w, 500, "Ошибка базы данных")
			return
		}
		out = append(out, u)
	}
	if rows.Err() != nil {
		fail(w, 503, "Не удалось завершить загрузку пользователей")
		return
	}
	write(w, 200, out)
}
func (a *API) addUser(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decode(w, r, &p) {
		return
	}
	if !validAccount(p.Username, p.Password) {
		fail(w, 400, "Имя: 3–80 символов. Пароль: не менее 12 символов")
		return
	}
	if p.Role != "admin" && p.Role != "operator" && p.Role != "viewer" {
		fail(w, 400, "Допустимые роли: admin, operator, viewer")
		return
	}
	u := model.User{ID: security.ID(), Username: p.Username, Role: p.Role}
	_, e := a.Store.Pool.Exec(r.Context(), `INSERT INTO mp_users(id,username,password_hash,role) VALUES($1,$2,$3,$4)`, u.ID, u.Username, security.PasswordHash(p.Password), u.Role)
	if e != nil {
		fail(w, 409, "Имя пользователя уже занято или запись не удалось сохранить")
		return
	}
	a.audit(r.Context(), actor(r), "user.create", u.ID, p.Role)
	write(w, 201, u)
}
