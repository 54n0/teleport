/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package web

import (
	"net/http"
	"time"

	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/httplib"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/web/ui"
	"github.com/gravitational/trace"

	"github.com/julienschmidt/httprouter"
)

// requestUser is used to unmarshal JSON requests.
// Requests are made from the web UI for:
//	- user creation
type requestUser struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// responseCreateUser is used to send back data
// about created local user and password setup token.
type responseCreateUser struct {
	User  ui.User       `json:"user"`
	Token ui.ResetToken `json:"token"`
}

// responseGetUsers is used to send back list of all locally saved users.
type responseGetUsers struct {
	Users []ui.User `json:"users"`
}

// responseUpdateUser is used to send back an updated user.
type responseUpdateUser struct {
	User ui.User `json:"user"`
}

// createUser allows a UI user to create a new user.
//
// POST /webapi/sites/:site/namespaces/:namespace/users
//
// Request:
// {
//		"username": "foo",
//		"roles": ["role1", "role2"]
// }
//
// Response:
// {
//		"user": {...},
//		"token": {...}
// }
func (h *Handler) createUser(w http.ResponseWriter, r *http.Request, p httprouter.Params, ctx *SessionContext, site reversetunnel.RemoteSite) (interface{}, error) {
	// Pull out request data.
	var req *requestUser
	if err := httplib.ReadJSON(r, &req); err != nil {
		return nil, trace.Wrap(err)
	}

	if err := req.checkAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	// Check user doesn't exist already.
	_, err := ctx.clt.GetUser(req.Name, false)
	if !trace.IsNotFound(err) {
		if err != nil {
			return nil, trace.Wrap(err, "failed to check whether user %q exists: %v", req.Name, err)
		}
		return nil, trace.BadParameter("user %q already registered", req.Name)
	}

	// Create and insert new user.
	newUser, err := services.NewUser(req.Name)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	newUser.SetRoles(req.Roles)
	newUser.SetCreatedBy(services.CreatedBy{
		User: services.UserRef{Name: ctx.user},
		Time: h.clock.Now().UTC(),
	})

	if err := ctx.clt.CreateUser(r.Context(), newUser); err != nil {
		return nil, trace.Wrap(err)
	}

	// Create sign up token.
	token, err := ctx.clt.CreateResetPasswordToken(r.Context(), auth.CreateResetPasswordTokenRequest{
		Name: req.Name,
		Type: auth.ResetPasswordTokenTypeInvite,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &responseCreateUser{
		User: ui.User{
			Name:    newUser.GetName(),
			Roles:   newUser.GetRoles(),
			Created: newUser.GetCreatedBy().Time,
		},
		Token: ui.ResetToken{
			Name:    token.GetUser(),
			Value:   token.GetMetadata().Name,
			Expires: token.Expiry().Sub(h.clock.Now().UTC()).Round(time.Second).String(),
		},
	}, nil
}

// getUsers allows a UI user to retrieve a list of all locally saved users.
//
// GET /webapi/sites/:site/namespaces/:namespace/users
//
// Response:
// {
//		"user": [{user1}, {user2}...]
// }
func (h *Handler) getUsers(w http.ResponseWriter, r *http.Request, p httprouter.Params, ctx *SessionContext, site reversetunnel.RemoteSite) (interface{}, error) {
	localUsers, err := ctx.clt.GetUsers(false)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Iterate each user into UI compatible model.
	var users []ui.User
	for _, u := range localUsers {
		user := ui.User{
			Name:    u.GetName(),
			Roles:   u.GetRoles(),
			Created: u.GetCreatedBy().Time,
		}
		users = append(users, user)
	}

	return &responseGetUsers{Users: users}, nil
}

// updateUser allows a UI user to update a existing user.
//
// PUT /webapi/sites/:site/namespaces/:namespace/users
//
// Request:
// {
//		"username": "foo",
//		"roles": ["role1", "role2"]
// }
//
// Response:
// {
//		"user": {...}
// }
func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request, p httprouter.Params, ctx *SessionContext, site reversetunnel.RemoteSite) (interface{}, error) {
	// Pull out request data.
	var req *requestUser
	if err := httplib.ReadJSON(r, &req); err != nil {
		return nil, trace.Wrap(err)
	}

	if err := req.checkAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	user, err := ctx.clt.GetUser(req.Name, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Update user fields.
	user.SetRoles(req.Roles)
	if err := ctx.clt.UpdateUser(r.Context(), user); err != nil {
		return nil, trace.Wrap(err)
	}

	return &responseUpdateUser{
		User: ui.User{
			Name:    user.GetName(),
			Roles:   user.GetRoles(),
			Created: user.GetCreatedBy().Time,
		},
	}, nil
}

// deleteUser allows a UI user to delete a existing user.
//
// DELETE /webapi/sites/:site/namespaces/:namespace/users/:username
//
// Response:
// {
//		"message": "ok"
// }
func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request, p httprouter.Params, ctx *SessionContext, site reversetunnel.RemoteSite) (interface{}, error) {
	username := p.ByName("username")
	if username == "" {
		return nil, trace.BadParameter("missing user name")
	}

	if err := ctx.clt.DeleteUser(r.Context(), username); err != nil {
		return nil, trace.Wrap(err)
	}

	return ok(), nil
}

// checkAndSetDefaults checks validity of a user request.
func (r *requestUser) checkAndSetDefaults() error {
	if r.Name == "" {
		return trace.BadParameter("missing user name")
	}
	if len(r.Roles) == 0 {
		return trace.BadParameter("missing roles")
	}
	return nil
}
