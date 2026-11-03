package main

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// resolveHistorySyncSender replica a lógica de resolução do sender no handler
// de HistorySync (wmiau.go), para testar isoladamente o fallback de
// WebMessageInfo.GetParticipant() quando MessageKey.GetParticipant() está vazio.
func resolveHistorySyncSender(chatJID types.JID, messageKey *waCommon.MessageKey, msg *waWeb.WebMessageInfo, isFromMe bool, accountOwnerJID string) string {
	var senderJID string

	if isFromMe {
		senderJID = accountOwnerJID
		if senderJID == "" {
			senderJID = "me"
		}
	} else {
		if chatJID.Server == types.GroupServer || chatJID.Server == types.BroadcastServer {
			senderJID = messageKey.GetParticipant()
			if senderJID == "" {
				senderJID = msg.GetParticipant()
			}
		} else {
			senderJID = chatJID.String()
		}
	}

	return senderJID
}

func TestHistorySyncSenderResolution(t *testing.T) {
	const ownerJID = "1234567890"

	cases := []struct {
		name         string
		chat         string
		keyPart      string
		infoPart     string
		fromMe       bool
		want         string
	}{
		{"group-key", "123@g.us", "200@lid", "", false, "200@lid"},
		{"group-lid", "123@g.us", "", "200@lid", false, "200@lid"},
		{"group-phone", "123@g.us", "", "200@s.whatsapp.net", false, "200@s.whatsapp.net"},
		{"key-precedence", "123@g.us", "200@lid", "300@lid", false, "200@lid"},
		{"broadcast", "status@broadcast", "", "200@lid", false, "200@lid"},
		{"missing-sender", "123@g.us", "", "", false, ""},
		{"outgoing", "123@g.us", "", "200@lid", true, ownerJID},
		{"direct", "300@s.whatsapp.net", "", "200@lid", false, "300@s.whatsapp.net"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatJID, err := types.ParseJID(tc.chat)
			if err != nil {
				t.Fatal(err)
			}

			messageKey := &waCommon.MessageKey{
				ID:          proto.String("test-id"),
				RemoteJID:   proto.String(tc.chat),
				FromMe:      proto.Bool(tc.fromMe),
				Participant: proto.String(tc.keyPart),
			}

			msg := &waWeb.WebMessageInfo{
				Key:         messageKey,
				Participant: proto.String(tc.infoPart),
			}

			got := resolveHistorySyncSender(chatJID, messageKey, msg, tc.fromMe, ownerJID)
			if got != tc.want {
				t.Fatalf("resolveHistorySyncSender = %q; want %q", got, tc.want)
			}
		})
	}
}