/*
Copyright 2014 Tamás Gulácsi

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

package imapclient

import (
	"crypto/sha1"
	"io"
	"strconv"
	"time"

	"github.com/tgulacsi/go/temp"
)

var (
	// ShortSleep is the duration which ised for sleep after successful delivery.
	ShortSleep = 1 * time.Second
	// LongSleep is the duration which used for sleep between errors and if the inbox is empty.
	LongSleep = 5 * time.Minute
)

// DeliveryLoop periodically checks the inbox for mails with the specified pattern
// in the subject (or for any unseen mail if pattern == ""), tries to parse the
// message, and call the deliver function with the parsed message.
//
// If deliver did not returned error, the message is marked as Seen, and if outbox
// is not empty, then moved to outbox.
//
// deliver is called with the message, where X-UID and X-SHA1 are set.
func DeliveryLoop(c Client, inbox, pattern string, deliver DeliverFunc, outbox, errbox string, closeCh <-chan struct{}) {
	if inbox == "" {
		inbox = "INBOX"
	}
	for {
		n, err := one(c, inbox, pattern, deliver, outbox, errbox)
		if err != nil {
			Log.Error("DeliveryLoop one round", "n", n, "error", err)
		} else {
			Log.Info("DeliveryLoop one round", "n", n)
		}
		select {
		case _, ok := <-closeCh:
			if !ok { //channel is closed
				return
			}
		default:
		}

		if err != nil {
			time.Sleep(LongSleep)
			continue
		}
		if n > 0 {
			time.Sleep(ShortSleep)
		} else {
			time.Sleep(LongSleep)
		}
		continue
	}
}

// DeliverOne does one round of message reading and delivery. Does not loop.
// Returns the number of messages delivered.
func DeliverOne(c Client, inbox, pattern string, deliver DeliverFunc, outbox, errbox string) (int, error) {
	if inbox == "" {
		inbox = "INBOX"
	}
	return one(c, inbox, pattern, deliver, outbox, errbox)
}

// DeliverFunc is the type for message delivery.
//
// r is the message data, uid is the IMAP server sent message UID, sha1 is the message's sha1 hash.
type DeliverFunc func(r io.ReadSeeker, uid uint32, sha1 []byte) error

func one(c Client, inbox, pattern string, deliver DeliverFunc, outbox, errbox string) (int, error) {
	if err := c.Connect(); err != nil {
		Log.Error("Connecting", "server", c, "error", err)
		return 0, err
	}
	defer c.Close(true)

	uids, err := c.List(inbox, pattern, outbox != "" && errbox != "")
	if err != nil {
		Log.Error("List", "server", c, "inbox", inbox, "error", err)
		return 0, err
	}

	var n int
	hsh := sha1.New()
	for _, uid := range uids {
		hsh.Reset()
		body := temp.NewMemorySlurper(strconv.FormatUint(uint64(uid), 10))
		if _, err = c.ReadTo(io.MultiWriter(body, hsh), uid); err != nil {
			Log.Error("Read", "uid", uid, "error", err)
			continue
		}

		err = deliver(body, uid, hsh.Sum(nil))
		body.Close()
		if err != nil {
			Log.Error("deliver", "uid", uid, "error", err)
			if errbox != "" {
				if err = c.Move(uid, errbox); err != nil {
					Log.Error("move", "uid", uid, "errbox", errbox, "error", err)
				}
			}
			continue
		}
		n++

		if err = c.MarkSeen(uid); err != nil {
			Log.Error("mark seen", "uid", uid, "error", err)
		}

		if outbox != "" {
			if err = c.Move(uid, outbox); err != nil {
				Log.Error("move", "uid", uid, "outbox", outbox, "error", err)
				continue
			}
		}
	}

	return n, nil
}
