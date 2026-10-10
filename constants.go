package main

// List of supported event types
var supportedEventTypes = []string{
	// Messages and Communication
	"Message",
	"UndecryptableMessage",
	"Receipt",
	"MediaRetry",
	"ReadReceipt",

	// Groups and Contacts
	"GroupInfo",
	"JoinedGroup",
	"Picture",
	"BlocklistChange",
	"Blocklist",

	// Connection and Session
	"Connected",
	"Disconnected",
	"ConnectFailure",
	"KeepAliveRestored",
	"KeepAliveTimeout",
	"QRTimeout",
	"PasskeyRequest",
	"LoggedOut",
	"ClientOutdated",
	"TemporaryBan",
	"StreamError",
	"StreamReplaced",
	"PairSuccess",
	"PairError",
	"QR",

	// Privacy and Settings
	"PrivacySettings",
	"PushNameSetting",
	"UserAbout",

	// Synchronization and State
	"AppState",
	"AppStateSyncComplete",
	"HistorySync",
	"OfflineSyncCompleted",
	"OfflineSyncPreview",

	// Calls
	"CallOffer",
	"CallAccept",
	"CallTerminate",
	"CallOfferNotice",
	"CallRelayLatency",

	// Presence and Activity
	"Presence",
	"ChatPresence",

	// Identity
	"IdentityChange",

	// Newsletter (WhatsApp Channels)
	"NewsletterJoin",
	"NewsletterLeave",
	"NewsletterMuteChange",
	"NewsletterLiveUpdate",

	// Facebook/Meta Bridge
	"FBMessage",

	// Special - receives all events
	"All",
}
