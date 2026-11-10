package main

import (
	"context"
	"errors"
	"testing"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

func TestJIDUserKey(t *testing.T) {
	tests := []struct {
		jid      string
		expected string
	}{
		{"55512345678@s.whatsapp.net", "55512345678"},
		{"55512345678:92@s.whatsapp.net", "55512345678"},
		{"55512345678:95@s.whatsapp.net", "55512345678"},
		{"1203236251234@c.us", "1203236251234"},
		{"1203236251234:10@c.us", "1203236251234"},
	}
	for _, tt := range tests {
		got := jidUserKey(tt.jid)
		if got != tt.expected {
			t.Errorf("jidUserKey(%q) = %q, want %q", tt.jid, got, tt.expected)
		}
	}
}

func TestDeviceLookupErrorsDoNotFallBack(t *testing.T) {
	failure := errors.New("device database unavailable")
	for _, exact := range []string{"", "15551234567:95@s.whatsapp.net"} {
		if device, err := findSessionDevice(context.Background(), unavailableDeviceStore{failure}, exact, "15551234567@s.whatsapp.net"); !errors.Is(err, failure) || device != nil {
			t.Fatalf("database failure masked: %v %v", device, err)
		}
	}
}

func TestSessionDeletionReservation(t *testing.T) {
	deletingSessionsMu.Lock()
	delete(deletingSessions, "test-user-reservation")
	deletingSessionsMu.Unlock()

	if err := reserveSessionDeletion("test-user-reservation"); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}

	if err := reserveSessionDeletion("test-user-reservation"); err == nil {
		t.Fatal("concurrent deletion allowed")
	}

	releaseSessionDeletion("test-user-reservation")

	if err := reserveSessionDeletion("test-user-reservation"); err != nil {
		t.Fatalf("reservation after release failed: %v", err)
	}

	releaseSessionDeletion("test-user-reservation")
}

func TestSessionIdempotent(t *testing.T) {
	ctx := context.Background()
	var stores sessionDeviceStore = &mockDeviceStore{devices: make(map[string]*store.Device)}

	device := &store.Device{ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net"}}
	stores.(*mockDeviceStore).devices["15551234567:95@s.whatsapp.net"] = device

	result, err := findSessionDevice(ctx, stores, "15551234567:95@s.whatsapp.net", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected device, got nil")
	}

	_, err = findSessionDevice(ctx, stores, "55599999999@invalid", "55512345678@s.whatsapp.net")
	if err == nil {
		t.Fatal("expected error for invalid JID")
	}
}

func TestFindSessionDeviceExactMatch(t *testing.T) {
	ctx := context.Background()
	stores := &mockDeviceStore{devices: make(map[string]*store.Device)}

	device1 := &store.Device{ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 92}}
	device2 := &store.Device{ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 95}}
	stores.devices["15551234567:92@s.whatsapp.net"] = device1
	stores.devices["15551234567:95@s.whatsapp.net"] = device2

	result, err := findSessionDevice(ctx, stores, "15551234567:95@s.whatsapp.net", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected device, got nil")
	}
	if result.ID.Device != 95 {
		t.Errorf("expected device 95, got %d", result.ID.Device)
	}
}

func TestDeleteSessionDevicesRemovesStaleDevicesForSoleOwner(t *testing.T) {
	ctx := context.Background()
	db := openDeletionTestDB(t, false)
	s := &server{db: db}
	seedDeletionUser(t, db, "only", "15551234567@s.whatsapp.net", "")

	stores := &mockDeviceStore{devices: map[string]*store.Device{
		"15551234567:92@s.whatsapp.net": {ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 92}},
		"15551234567:95@s.whatsapp.net": {ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 95}},
	}}

	if err := s.deleteSessionDevicesForAccount(ctx, "only", "", "15551234567@s.whatsapp.net", stores); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stores.devices) != 0 {
		t.Fatalf("devices remaining = %d; want 0", len(stores.devices))
	}
}

func TestDeleteSessionDevicesRefusesWhenAccountShared(t *testing.T) {
	ctx := context.Background()
	db := openDeletionTestDB(t, false)
	s := &server{db: db}
	seedDeletionUser(t, db, "only", "15551234567@s.whatsapp.net", "")
	seedDeletionUser(t, db, "other", "15551234567@s.whatsapp.net", "")

	stores := &mockDeviceStore{devices: map[string]*store.Device{
		"15551234567:92@s.whatsapp.net": {ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 92}},
		"15551234567:95@s.whatsapp.net": {ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 95}},
	}}

	err := s.deleteSessionDevicesForAccount(ctx, "only", "", "15551234567@s.whatsapp.net", stores)
	if !errors.Is(err, errAmbiguousSessionDevice) {
		t.Fatalf("err = %v; want ambiguity", err)
	}
	if len(stores.devices) != 2 {
		t.Fatalf("devices remaining = %d; want 2", len(stores.devices))
	}
}

func TestDeleteSessionDevicesExactDeletesOnlyExact(t *testing.T) {
	ctx := context.Background()
	db := openDeletionTestDB(t, false)
	s := &server{db: db}
	seedDeletionUser(t, db, "only", "15551234567@s.whatsapp.net", "15551234567:95@s.whatsapp.net")

	stores := &mockDeviceStore{devices: map[string]*store.Device{
		"15551234567:92@s.whatsapp.net": {ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 92}},
		"15551234567:95@s.whatsapp.net": {ID: &types.JID{User: "15551234567", Server: "s.whatsapp.net", Device: 95}},
	}}

	if err := s.deleteSessionDevicesForAccount(ctx, "only", "15551234567:95@s.whatsapp.net", "15551234567@s.whatsapp.net", stores); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := stores.devices["15551234567:95@s.whatsapp.net"]; ok {
		t.Fatal("exact device was not deleted")
	}
	if _, ok := stores.devices["15551234567:92@s.whatsapp.net"]; !ok {
		t.Fatal("non-exact device should have been preserved")
	}
}

type unavailableDeviceStore struct{ err error }

func (s unavailableDeviceStore) GetDevice(context.Context, types.JID) (*store.Device, error) {
	return nil, s.err
}
func (s unavailableDeviceStore) GetAllDevices(context.Context) ([]*store.Device, error) {
	return nil, s.err
}
func (s unavailableDeviceStore) DeleteDevice(context.Context, *store.Device) error {
	return s.err
}

type mockDeviceStore struct {
	devices map[string]*store.Device
}

func (m *mockDeviceStore) GetDevice(ctx context.Context, jid types.JID) (*store.Device, error) {
	device, ok := m.devices[jid.String()]
	if !ok {
		return nil, errors.New("device not found")
	}
	return device, nil
}

func (m *mockDeviceStore) GetAllDevices(ctx context.Context) ([]*store.Device, error) {
	devices := make([]*store.Device, 0, len(m.devices))
	for _, d := range m.devices {
		devices = append(devices, d)
	}
	return devices, nil
}

func (m *mockDeviceStore) DeleteDevice(ctx context.Context, device *store.Device) error {
	if device == nil || device.ID == nil {
		return errors.New("device ID must be set")
	}
	key := device.ID.String()
	if _, ok := m.devices[key]; !ok {
		return errors.New("device not found")
	}
	delete(m.devices, key)
	return nil
}
