package workbuddy

// International desktop identity is shared by chat, catalog and login.
// The embedded CLI uses WorkBuddy as its platform and workbuddy-ai as its
// application name. Electron side-channel requests use a different UA;
// do not replace the capitalized first segment with that Electron identity.
const (
	clientVersion = "5.8.4"
	cliVersion    = "2.161.0"
	clientUA      = "WorkBuddy/" + clientVersion + " workbuddy-ai/" + clientVersion + " CLI/" + cliVersion
	billingUA     = "WorkBuddy/" + clientVersion
)
