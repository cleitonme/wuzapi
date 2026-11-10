package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

var errAmbiguousSessionDevice = errors.New("multiple stored devices match an account shared by another user; exact device ownership must be established before reconnecting or deleting")

type sessionDeviceStore interface {
	GetDevice(context.Context, types.JID) (*store.Device, error)
	GetAllDevices(context.Context) ([]*store.Device, error)
	DeleteDevice(context.Context, *store.Device) error
}

func findSessionDevice(ctx context.Context, stores sessionDeviceStore, exact, account string) (*store.Device, error) {
	devices, err := findSessionDevices(ctx, stores, exact, account)
	if err != nil {
		return nil, err
	}
	switch len(devices) {
	case 0:
		return nil, nil
	case 1:
		return devices[0], nil
	default:
		return nil, errAmbiguousSessionDevice
	}
}

// findSessionDevices devolve todos os devices que podem representar a conta.
// O caminho exato/device continua determinístico; o caminho por conta sem
// device pode retornar mais de um resultado, deixando a decisão para o
// chamador (conexão recusa, exclusão tolera sobras do próprio canal).
func findSessionDevices(ctx context.Context, stores sessionDeviceStore, exact, account string) ([]*store.Device, error) {
	if exact != "" {
		jid, err := types.ParseJID(exact)
		if err != nil || jid.IsEmpty() {
			return nil, fmt.Errorf("invalid saved device JID %q", exact)
		}
		device, err := stores.GetDevice(ctx, jid)
		if err != nil {
			return nil, err
		}
		if device == nil {
			return nil, nil
		}
		return []*store.Device{device}, nil
	}
	if account == "" {
		return nil, nil
	}
	jid, ok := parseJID(account)
	if !ok || jid.IsEmpty() {
		return nil, fmt.Errorf("invalid saved account JID %q", account)
	}
	if jid.Device != 0 || jid.RawAgent != 0 {
		device, err := stores.GetDevice(ctx, jid)
		if err != nil {
			return nil, err
		}
		if device == nil {
			return nil, nil
		}
		return []*store.Device{device}, nil
	}
	devices, err := stores.GetAllDevices(ctx)
	if err != nil {
		return nil, err
	}
	var matches []*store.Device
	for _, device := range devices {
		if device.ID == nil || jidUserKey(device.ID.String()) != jidUserKey(account) {
			continue
		}
		matches = append(matches, device)
	}
	return matches, nil
}

func (s *server) sessionDeviceIdentity(ctx context.Context, userID string) (string, string, error) {
	var exact, account string
	err := s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(device_jid,''), COALESCE(jid,'') FROM users WHERE id=?`), userID).Scan(&exact, &account)
	return exact, account, err
}

func (s *server) checkDeviceOwner(ctx context.Context, userID, deviceJID string, legacy bool) error {
	var claimed bool
	jid, err := types.ParseJID(deviceJID)
	if err != nil {
		return err
	}
	err = s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT EXISTS(SELECT 1 FROM users WHERE id<>? AND (device_jid=? OR (? AND COALESCE(device_jid,'')='' AND jid=?)))`), userID, deviceJID, legacy, jid.ToNonAD().String()).Scan(&claimed)
	if err != nil {
		return err
	}
	if claimed {
		return errors.New("stored device belongs to another user")
	}
	return nil
}

func (s *server) resolveDeviceStore(ctx context.Context, userID string) (*store.Device, error) {
	if container == nil {
		return nil, errors.New("device store is unavailable")
	}
	exact, account, err := s.sessionDeviceIdentity(ctx, userID)
	if err != nil {
		return nil, err
	}
	device, err := findSessionDevice(ctx, container, exact, account)
	if err != nil {
		return nil, err
	}
	if device == nil {
		return container.NewDevice(), nil
	}
	if err := s.checkDeviceOwner(ctx, userID, device.ID.String(), exact == "" && account != device.ID.String()); err != nil {
		return nil, err
	}
	result, err := s.db.ExecContext(ctx, s.db.Rebind(`UPDATE users SET device_jid=? WHERE id=?`), device.ID.String(), userID)
	if err != nil {
		return nil, fmt.Errorf("persist device ownership: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return nil, errors.New("user no longer exists")
	}
	return device, nil
}

// accountSharedByOtherUser informa se outro usuário do banco usa a mesma conta
// (mesmo "user key" do JID). Nesse caso credenciais duplicadas não podem ser
// escolhidas nem removidas em bloco, pois podem pertencer ao outro canal.
func (s *server) accountSharedByOtherUser(ctx context.Context, userID, account string) (bool, error) {
	key := jidUserKey(account)
	if key == "" {
		return false, nil
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`SELECT COALESCE(jid,''), COALESCE(device_jid,'') FROM users WHERE id<>?`), userID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var jid, deviceJID string
		if err := rows.Scan(&jid, &deviceJID); err != nil {
			return false, err
		}
		if jidUserKey(jid) == key || jidUserKey(deviceJID) == key {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *server) deleteSessionDevice(ctx context.Context, userID string) error {
	exact, account, err := s.sessionDeviceIdentity(ctx, userID)
	if err != nil {
		return err
	}
	if exact == "" && account == "" {
		return nil
	}
	if container == nil {
		return errors.New("device store is unavailable")
	}
	return s.deleteSessionDevicesForAccount(ctx, userID, exact, account, container)
}

func (s *server) deleteSessionDevicesForAccount(ctx context.Context, userID, exact, account string, stores sessionDeviceStore) error {
	devices, err := findSessionDevices(ctx, stores, exact, account)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return nil
	}
	if len(devices) > 1 {
		shared, err := s.accountSharedByOtherUser(ctx, userID, account)
		if err != nil {
			return err
		}
		if shared {
			return errAmbiguousSessionDevice
		}
	}
	var firstErr error
	for _, device := range devices {
		if device.ID == nil {
			continue
		}
		if err := s.checkDeviceOwner(ctx, userID, device.ID.String(), exact == "" && account != device.ID.String()); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := stores.DeleteDevice(ctx, device); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func jidUserKey(jid string) string {
	if jid == "" {
		return ""
	}
	userPart := strings.SplitN(jid, "@", 2)[0]
	return strings.SplitN(userPart, ":", 2)[0]
}
