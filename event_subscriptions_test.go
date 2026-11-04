package main

import (
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestResolveConnectEvents(t *testing.T) {
	cases := []struct {
		name        string
		subscribe   []string
		existing    string
		wantEvents  string
		wantChanged bool
	}{
		{"no subscribe preserves existing", nil, "Message,ReadReceipt", "Message,ReadReceipt", false},
		{"empty subscribe preserves existing", []string{}, "Message", "Message", false},
		{"no subscribe, no existing stays empty", nil, "", "", false},
		{"valid subscribe replaces and flags change", []string{"Message", "ReadReceipt"}, "Old", "Message,ReadReceipt", true},
		{"unsupported types are filtered", []string{"Message", "Bogus"}, "", "Message", true},
		{"duplicates are de-duplicated", []string{"Message", "Message"}, "", "Message", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEvents, gotChanged := resolveConnectEvents(tc.subscribe, tc.existing)
			if gotEvents != tc.wantEvents || gotChanged != tc.wantChanged {
				t.Fatalf("resolveConnectEvents(%v, %q) = (%q, %v); want (%q, %v)",
					tc.subscribe, tc.existing, gotEvents, gotChanged, tc.wantEvents, tc.wantChanged)
			}
		})
	}
}

func TestSetDisconnectedState(t *testing.T) {
	const id1 = "user-305-preserve"
	const id2 = "user-305-clear"

	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT, token TEXT, events TEXT, connected INTEGER)`); err != nil {
		t.Fatal(err)
	}

	seed := func(t *testing.T, s *server, id string) {
		t.Helper()
		if _, err := s.db.Exec(
			`INSERT INTO users (id, name, token, events, connected) VALUES ($1,$2,$3,$4,$5)`,
			id, "tester", "tok-305", "Message,ReadReceipt", 1); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	read := func(t *testing.T, s *server, id string) (events string, connected int) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT events, connected FROM users WHERE id=$1`, id).Scan(&events, &connected); err != nil {
			t.Fatalf("read user: %v", err)
		}
		return events, connected
	}

	t.Run("default preserves events", func(t *testing.T) {
		s := &server{db: db}
		seed(t, s, id1)
		if err := s.setDisconnectedState(id1, false); err != nil {
			t.Fatalf("setDisconnectedState: %v", err)
		}
		events, connected := read(t, s, id1)
		if events != "Message,ReadReceipt" {
			t.Errorf("events = %q; want preserved %q", events, "Message,ReadReceipt")
		}
		if connected != 0 {
			t.Errorf("connected = %d; want 0", connected)
		}
	})

	t.Run("clear=true resets events", func(t *testing.T) {
		s := &server{db: db}
		seed(t, s, id2)
		if err := s.setDisconnectedState(id2, true); err != nil {
			t.Fatalf("setDisconnectedState: %v", err)
		}
		events, connected := read(t, s, id2)
		if events != "" {
			t.Errorf("events = %q; want cleared %q", events, "")
		}
		if connected != 0 {
			t.Errorf("connected = %d; want 0", connected)
		}
	})
}