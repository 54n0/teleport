/*
Copyright 2015 Gravitational, Inc.

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

package session

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/lite"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
)

func TestSessions(t *testing.T) {
	utils.InitLoggerForTests(testing.Verbose())
	s := NewSessionSuite(t)

	t.Cleanup(func() {
		TearDownSessionSuite(s, t)
	})

	t.Run("TestID", func(t *testing.T) { s.TestID(t) })
	t.Run("TestSessionsCRUD", func(t *testing.T) { s.TestSessionsCRUD(t) })
	t.Run("TestSessionsInactivity", func(t *testing.T) { s.TestSessionsInactivity(t) })
	t.Run("TestPartiesCRUD", func(t *testing.T) { s.TestPartiesCRUD(t) })
}

type SessionSuite struct {
	dir   string
	srv   *server
	bk    backend.Backend
	clock clockwork.FakeClock
}

func NewSessionSuite(t *testing.T) *SessionSuite {
	var err error
	s := &SessionSuite{}

	s.clock = clockwork.NewFakeClockAt(time.Date(2016, 9, 8, 7, 6, 5, 0, time.UTC))
	s.dir = t.TempDir()
	s.bk, err = lite.NewWithConfig(context.TODO(),
		lite.Config{
			Path:  s.dir,
			Clock: s.clock,
		},
	)
	require.NoError(t, err)

	srv, err := New(s.bk)
	require.NoError(t, err)
	srv.(*server).clock = s.clock
	s.srv = srv.(*server)
	return s
}

func TearDownSessionSuite(s *SessionSuite, t *testing.T) {
	require.NoError(t, s.bk.Close())
}

func (s *SessionSuite) TestID(t *testing.T) {
	id := NewID()
	id2, err := ParseID(id.String())
	require.NoError(t, err)
	require.Equal(t, id, *id2)

	for _, val := range []string{"garbage", "", "   ", string(id) + "extra"} {
		id := ID(val)
		require.Error(t, id.Check())
	}
}

func (s *SessionSuite) TestSessionsCRUD(t *testing.T) {
	out, err := s.srv.GetSessions(defaults.Namespace)
	require.NoError(t, err)
	require.Equal(t, len(out), 0)

	// Create session.
	sess := Session{
		ID:             NewID(),
		Namespace:      defaults.Namespace,
		TerminalParams: TerminalParams{W: 100, H: 100},
		Login:          "bob",
		LastActive:     s.clock.Now().UTC(),
		Created:        s.clock.Now().UTC(),
	}
	require.NoError(t, s.srv.CreateSession(sess))

	// Make sure only one session exists.
	out, err = s.srv.GetSessions(defaults.Namespace)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(out, []Session{sess}))

	// Make sure the session is the one created above.
	s2, err := s.srv.GetSession(defaults.Namespace, sess.ID)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(s2, &sess))

	// Update session terminal parameter
	err = s.srv.UpdateSession(UpdateRequest{
		ID:             sess.ID,
		Namespace:      defaults.Namespace,
		TerminalParams: &TerminalParams{W: 101, H: 101},
	})
	require.NoError(t, err)

	// Verify update was applied.
	sess.TerminalParams = TerminalParams{W: 101, H: 101}
	s2, err = s.srv.GetSession(defaults.Namespace, sess.ID)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(s2, &sess))

	// Remove the session.
	err = s.srv.DeleteSession(defaults.Namespace, sess.ID)
	require.NoError(t, err)

	// Make sure session no longer exists.
	_, err = s.srv.GetSession(defaults.Namespace, sess.ID)
	require.Error(t, err)
}

// TestSessionsInactivity makes sure that session will be marked
// as inactive after period of inactivity
func (s *SessionSuite) TestSessionsInactivity(t *testing.T) {
	sess := Session{
		ID:             NewID(),
		Namespace:      defaults.Namespace,
		TerminalParams: TerminalParams{W: 100, H: 100},
		Login:          "bob",
		LastActive:     s.clock.Now().UTC(),
		Created:        s.clock.Now().UTC(),
	}
	require.NoError(t, s.srv.CreateSession(sess))

	// move forward in time:
	s.clock.Advance(defaults.ActiveSessionTTL + time.Second)

	// should not be in active sessions:
	s2, err := s.srv.GetSession(defaults.Namespace, sess.ID)
	require.Error(t, err)
	require.True(t, trace.IsNotFound(err))
	require.Nil(t, s2)
}

func (s *SessionSuite) TestPartiesCRUD(t *testing.T) {
	// create session:
	sess := Session{
		ID:             NewID(),
		Namespace:      defaults.Namespace,
		TerminalParams: TerminalParams{W: 100, H: 100},
		Login:          "vincent",
		LastActive:     s.clock.Now().UTC(),
		Created:        s.clock.Now().UTC(),
	}
	err := s.srv.CreateSession(sess)
	require.NoError(t, err)
	// add two people:
	parties := []Party{
		{
			ID:         NewID(),
			RemoteAddr: "1_remote_addr",
			User:       "first",
			ServerID:   "luna",
			LastActive: s.clock.Now().UTC(),
		},
		{
			ID:         NewID(),
			RemoteAddr: "2_remote_addr",
			User:       "second",
			ServerID:   "luna",
			LastActive: s.clock.Now().UTC(),
		},
	}
	err = s.srv.UpdateSession(UpdateRequest{
		ID:        sess.ID,
		Namespace: defaults.Namespace,
		Parties:   &parties,
	})
	require.NoError(t, err)
	// verify they're in the session:
	copy, err := s.srv.GetSession(defaults.Namespace, sess.ID)
	require.NoError(t, err)
	require.Equal(t, len(copy.Parties), 2)

	// empty update (list of parties must not change)
	err = s.srv.UpdateSession(UpdateRequest{ID: sess.ID, Namespace: defaults.Namespace})
	require.NoError(t, err)
	copy, _ = s.srv.GetSession(defaults.Namespace, sess.ID)
	require.Equal(t, len(copy.Parties), 2)

	// remove the 2nd party:
	deleted := copy.RemoveParty(parties[1].ID)
	require.True(t, deleted)
	err = s.srv.UpdateSession(UpdateRequest{ID: copy.ID, Parties: &copy.Parties, Namespace: defaults.Namespace})
	require.NoError(t, err)
	copy, _ = s.srv.GetSession(defaults.Namespace, sess.ID)
	require.Equal(t, len(copy.Parties), 1)

	// we still have the 1st party in:
	require.Equal(t, parties[0].ID, copy.Parties[0].ID)
}
