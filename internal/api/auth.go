package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/aluzed/freebox-proxystream/internal/db"
)

const minPasswordLen = 8

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		// Secure is deliberately not set: the service is reached over
		// Tailscale on plain HTTP, and a Secure cookie would never be sent.
	})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	has, err := s.store.HasUser(r.Context())
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if !has {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if s.authenticate(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login.html", map[string]any{
		"Title": "Connexion",
		"Next":  r.URL.Query().Get("next"),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}

	user, err := s.store.Authenticate(r.Context(), username, password)
	if errors.Is(err, db.ErrBadCredentials) {
		s.log.Warn("failed login attempt", "username", username, "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login.html", map[string]any{
			"Title": "Connexion",
			"Error": "Identifiant ou mot de passe incorrect.",
			"Next":  next,
		})
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	token, _, err := s.store.NewSession(r.Context(), user.ID, s.cfg.SessionTTL)
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, token, int(s.cfg.SessionTTL.Seconds()))
	s.log.Info("login", "username", user.Username)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(r.Context(), c.Value)
	}
	s.setSessionCookie(w, "", -1)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleSetupPage is the first-run account creation screen. It disappears as
// soon as an account exists.
func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	has, err := s.store.HasUser(r.Context())
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if has {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, "setup.html", map[string]any{"Title": "Premier démarrage"})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	has, err := s.store.HasUser(r.Context())
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if has {
		http.Error(w, "an account already exists", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	fail := func(msg string) {
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "setup.html", map[string]any{
			"Title": "Premier démarrage", "Error": msg, "Username": username,
		})
	}
	switch {
	case username == "":
		fail("Choisissez un identifiant.")
		return
	case len(password) < minPasswordLen:
		fail("Le mot de passe doit faire au moins 8 caractères.")
		return
	case password != confirm:
		fail("Les deux mots de passe ne correspondent pas.")
		return
	}

	hash, err := db.HashPassword(password)
	if err != nil {
		http.Error(w, "could not hash password", http.StatusInternalServerError)
		return
	}
	user, err := s.store.CreateUser(r.Context(), username, hash)
	if err != nil {
		http.Error(w, "could not create account", http.StatusInternalServerError)
		return
	}

	token, _, err := s.store.NewSession(r.Context(), user.ID, s.cfg.SessionTTL)
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, token, int(s.cfg.SessionTTL.Seconds()))
	s.log.Info("initial account created", "username", username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) fragChangePassword(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil || user.ID == 0 {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Le changement de mot de passe nécessite une session."})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderFragment(w, r, "alert", map[string]any{"Level": "error", "Message": "Formulaire invalide."})
		return
	}

	current := r.FormValue("current_password")
	next := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	fail := func(msg string) {
		s.renderFragment(w, r, "alert", map[string]any{"Level": "error", "Message": msg})
	}
	switch {
	case len(next) < minPasswordLen:
		fail("Le nouveau mot de passe doit faire au moins 8 caractères.")
		return
	case next != confirm:
		fail("Les deux mots de passe ne correspondent pas.")
		return
	}

	err := s.store.ChangePassword(r.Context(), user.ID, current, next)
	if errors.Is(err, db.ErrBadCredentials) {
		fail("Mot de passe actuel incorrect.")
		return
	}
	if err != nil {
		fail("Erreur lors du changement de mot de passe.")
		return
	}

	// Every session was just invalidated, including this one.
	s.setSessionCookie(w, "", -1)
	w.Header().Set("HX-Redirect", "/login")
	s.renderFragment(w, r, "alert", map[string]any{
		"Level": "success", "Message": "Mot de passe modifié, reconnectez-vous."})
}
