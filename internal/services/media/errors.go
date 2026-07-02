package media

import "errors"

// ErrSIPCallInProgress reports that the intercom refused an outgoing INVITE
// with 486 Busy Here — i.e. a call is already active. The composite backend
// treats this as authoritative evidence that the AV pipeline is already armed
// and downgrades to AV add-stream commands only.
var ErrSIPCallInProgress = errors.New("sip call already in progress (486)")
