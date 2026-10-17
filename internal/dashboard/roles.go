package dashboard

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/store"
)

var roleNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

var roleNotices = map[string]string{
	"created": "Role created.",
	"updated": "Role updated. It applies to users within a few seconds.",
	"deleted": "Role deleted.",
}

type roleRow struct {
	Name, Kind string
	Actions    []authz.Action
	Users      int
	Builtin    bool
	Editable   bool
	Boxes      []actionBox
}

type actionBox struct {
	Action  authz.Action
	Checked bool
}

// loadRoles is the authz cache's source: the actions of every custom role.
// Built-in roles are not read from the table.
func (d *Dashboard) loadRoles(ctx context.Context) (map[authz.Role][]authz.Action, error) {
	rows, err := d.o.DB.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	out := map[authz.Role][]authz.Action{}
	for _, r := range rows {
		if r.Builtin {
			continue
		}
		var acts []authz.Action
		for _, a := range r.Actions {
			if authz.ValidAction(authz.Action(a)) { // an action removed from the enum grants nothing
				acts = append(acts, authz.Action(a))
			}
		}
		out[authz.Role(r.Name)] = acts
	}
	return out, nil
}

// roleNames lists the roles a user can hold: built-ins first, then custom ones.
func (d *Dashboard) roleNames(ctx context.Context) ([]string, error) {
	names := slices.Clone(store.BuiltinRoleNames)
	rows, err := d.o.DB.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if !r.Builtin && !slices.Contains(names, r.Name) {
			names = append(names, r.Name)
		}
	}
	return names, nil
}

func (d *Dashboard) rolesPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderRoles(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderRoles(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	stored, err := d.o.DB.ListRoles(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	counts, err := d.o.DB.RoleUserCounts(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	var rows []roleRow
	for _, name := range store.BuiltinRoleNames {
		acts := authz.BuiltinRoles()[authz.Role(name)]
		rows = append(rows, roleRow{Name: name, Kind: "built-in", Actions: acts, Users: counts[name], Builtin: true})
	}
	for _, sr := range stored {
		if sr.Builtin {
			continue
		}
		row := roleRow{Name: sr.Name, Kind: "custom", Users: counts[sr.Name], Editable: true}
		for _, a := range sr.Actions {
			if authz.ValidAction(authz.Action(a)) {
				row.Actions = append(row.Actions, authz.Action(a))
			}
		}
		for _, a := range authz.Grantable() {
			row.Boxes = append(row.Boxes, actionBox{a, slices.Contains(row.Actions, a)})
		}
		rows = append(rows, row)
	}
	v := d.shell(r, s, "roles", "Roles")
	v.Error, v.Notice = errMsg, roleNotices[r.URL.Query().Get("n")]
	v.RoleRows = rows
	for _, a := range authz.Grantable() {
		v.NewBoxes = append(v.NewBoxes, actionBox{Action: a})
	}
	d.render(w, status, "dash_roles", v)
}

// formActions reads the ticked actions, refusing anything not grantable.
func formActions(r *http.Request) ([]string, error) {
	var out []string
	for _, a := range r.PostForm["action"] {
		if !slices.Contains(authz.Grantable(), authz.Action(a)) {
			return nil, errors.New("unknown or ungrantable action " + a)
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (d *Dashboard) roleCreate(w http.ResponseWriter, r *http.Request, s *session) {
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if !roleNameRE.MatchString(name) {
		d.renderRoles(w, r, s, http.StatusBadRequest, "Role names are 2-32 characters: lowercase letters, digits, - and _, starting with a letter.")
		return
	}
	actions, err := formActions(r)
	if err != nil {
		d.renderRoles(w, r, s, http.StatusBadRequest, err.Error())
		return
	}
	switch err := d.o.DB.CreateRole(r.Context(), name, actions, d.o.Now()); {
	case errors.Is(err, store.ErrBuiltinRole):
		d.renderRoles(w, r, s, http.StatusBadRequest, "That name belongs to a built-in role.")
		return
	case errors.Is(err, store.ErrDuplicate):
		d.renderRoles(w, r, s, http.StatusBadRequest, "A role with that name already exists; edit it instead.")
		return
	case err != nil:
		d.fail(w, err)
		return
	}
	d.roles.Invalidate()
	if err := d.record(r.Context(), r, s, "role.create", name, nil, map[string]any{"actions": actions}); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/roles?n=created", http.StatusSeeOther)
}

// customRole loads the stored role named in the URL, answering 404/400 itself
// for a missing or built-in one.
func (d *Dashboard) customRole(w http.ResponseWriter, r *http.Request, s *session) (store.RoleRow, bool) {
	name := r.PathValue("name")
	if slices.Contains(store.BuiltinRoleNames, name) {
		d.renderRoles(w, r, s, http.StatusBadRequest, "Built-in roles cannot be changed or deleted.")
		return store.RoleRow{}, false
	}
	rows, err := d.o.DB.ListRoles(r.Context())
	if err != nil {
		d.fail(w, err)
		return store.RoleRow{}, false
	}
	for _, row := range rows {
		if row.Name == name && !row.Builtin {
			return row, true
		}
	}
	http.NotFound(w, r)
	return store.RoleRow{}, false
}

func (d *Dashboard) roleUpdate(w http.ResponseWriter, r *http.Request, s *session) {
	old, ok := d.customRole(w, r, s)
	if !ok {
		return
	}
	actions, err := formActions(r)
	if err != nil {
		d.renderRoles(w, r, s, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := d.o.DB.SaveRole(r.Context(), old.Name, actions, d.o.Now()); err != nil {
		d.fail(w, err)
		return
	}
	d.roles.Invalidate()
	if err := d.record(r.Context(), r, s, "role.update", old.Name, map[string]any{"actions": old.Actions}, map[string]any{"actions": actions}); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/roles?n=updated", http.StatusSeeOther)
}

func (d *Dashboard) roleDelete(w http.ResponseWriter, r *http.Request, s *session) {
	old, ok := d.customRole(w, r, s)
	if !ok {
		return
	}
	switch err := d.o.DB.DeleteRole(r.Context(), old.Name); {
	case errors.Is(err, store.ErrRoleInUse):
		d.renderRoles(w, r, s, http.StatusBadRequest, "That role is assigned to users; give them another role first.")
		return
	case err != nil:
		d.fail(w, err)
		return
	}
	d.roles.Invalidate()
	if err := d.record(r.Context(), r, s, "role.delete", old.Name, map[string]any{"actions": old.Actions}, nil); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/roles?n=deleted", http.StatusSeeOther)
}
