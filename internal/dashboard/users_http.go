package dashboard

import (
	"errors"
	"net/http"
	"strings"

	"github.com/paymux/paymux/internal/authz"

	"github.com/paymux/paymux/internal/store"
)

var notices = map[string]string{
	"created":  "User created. They will set up two-factor authentication at first login.",
	"role":     "Role updated.",
	"disabled": "User disabled and signed out.",
	"enabled":  "User enabled.",
}

func (d *Dashboard) usersPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderUsers(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderUsers(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	list, err := d.o.DB.ListUsers(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "users", "Users")
	v.Error, v.Notice = errMsg, notices[r.URL.Query().Get("n")]
	v.CanManage = s.can(authz.UserManage)
	v.Roles = Roles
	for _, u := range list {
		v.Users = append(v.Users, userRow{ID: u.ID, Email: u.Email, Role: u.Role, Disabled: u.Disabled,
			Enrolled: u.TOTPLastStep >= 0, Created: u.CreatedAt.UTC().Format("2006-01-02"), IsSelf: u.ID == s.User.ID})
	}
	d.render(w, status, "dash_users", v)
}

func (d *Dashboard) userCreate(w http.ResponseWriter, r *http.Request, s *session) {
	u, err := d.users.Create(r.Context(), r.PostForm.Get("email"), r.PostForm.Get("role"), r.PostForm.Get("password"))
	if errors.Is(err, ErrInvalid) {
		d.renderUsers(w, r, s, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(r.Context(), r, s, "user.create", u.ID, nil, map[string]string{"email": u.Email, "role": u.Role}); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/users?n=created", http.StatusSeeOther)
}

// target loads the user named in the URL, answering 404 itself when missing.
func (d *Dashboard) target(w http.ResponseWriter, r *http.Request) (store.UserRow, bool) {
	u, err := d.o.DB.UserByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return u, false
	}
	if err != nil {
		d.fail(w, err)
		return u, false
	}
	return u, true
}

func (d *Dashboard) userRole(w http.ResponseWriter, r *http.Request, s *session) {
	u, ok := d.target(w, r)
	if !ok {
		return
	}
	role := r.PostForm.Get("role")
	switch {
	case roleRank(role) == 0:
		d.renderUsers(w, r, s, http.StatusBadRequest, "Role must be admin, operator or viewer.")
		return
	case u.ID == s.User.ID:
		d.renderUsers(w, r, s, http.StatusBadRequest, "You cannot change your own role.")
		return
	}
	if err := d.o.DB.SetUserRole(r.Context(), u.ID, role); err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(r.Context(), r, s, "user.role", u.ID, map[string]string{"role": u.Role}, map[string]string{"role": role}); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/users?n=role", http.StatusSeeOther)
}

func (d *Dashboard) userDisable(w http.ResponseWriter, r *http.Request, s *session) {
	u, ok := d.target(w, r)
	if !ok {
		return
	}
	disable := r.PostForm.Get("action") != "enable"
	if disable && u.ID == s.User.ID {
		d.renderUsers(w, r, s, http.StatusBadRequest, "You cannot disable your own account.")
		return
	}
	if err := d.o.DB.SetUserDisabled(r.Context(), u.ID, disable); err != nil {
		d.fail(w, err)
		return
	}
	n := "enabled"
	if disable {
		n = "disabled"
		if err := d.o.DB.DeleteUserSessions(r.Context(), u.ID); err != nil {
			d.fail(w, err)
			return
		}
	}
	action := "user.enable"
	if disable {
		action = "user.disable"
	}
	if err := d.record(r.Context(), r, s, action, u.ID, nil, nil); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/users?n="+n, http.StatusSeeOther)
}
