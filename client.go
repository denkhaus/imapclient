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

// Package imapclient is for listing folders, reading messages
// and moving them around (delete, unread, move).
package imapclient

import (
	"crypto/tls"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/mxk/go-imap/imap"
	"github.com/tgulacsi/go/loghlp"
	"gopkg.in/inconshreveable/log15.v2"
)

var (
	// Log uses DiscardHandler (produces no output) by default.
	Log = log15.New("lib", "imapclient")

	// Timeout is the client timeout - 30 seconds by default.
	Timeout = 30 * time.Second

	// TLSConfig is the client's config for DialTLS.
	TLSConfig = tls.Config{InsecureSkipVerify: true}
)

func init() {
	Log.SetHandler(log15.DiscardHandler())
}

func Inspect(args ...interface{}) {
	spew.Dump(args)
}

// Client interface declares the needed methods for listing messages,
// deleting and moving them around.
type Client interface {
	Connect() error
	Close(commit bool) error
	List(mbox, pattern string, all bool) ([]uint32, error)
	ReadTo(w io.Writer, msgID uint32) (int64, error)
	GetFlags(msgID uint32) (imap.FlagSet, error)
	SetFlag(msgID uint32, keyword string, st bool) error
	SetFlagRegex(msgID uint32, regex string, st bool) error
	MarkSeen(msgID uint32) error
	MarkUnseen(msgID uint32) error
	MarkDeleted(msgID uint32) error
	MarkUndeleted(msgID uint32) error
	Move(msgID uint32, mbox string) error
	SetLogMask(mask imap.LogMask) imap.LogMask
}

const (
	noTLS    = -1
	maybeTLS = 0
	forceTLS = 1
)

type client struct {
	host, username, password string
	port, tls                int
	noUTF8                   bool
	c                        *imap.Client
	created                  []string
}

// NewClient returns a new (not connected) Client, using TLS iff port == 143.
func NewClient(host string, port int, username, password string) Client {
	if port == 0 {
		port = 143
	}
	if port == 143 {
		return NewClientNoTLS(host, port, username, password)
	}
	return NewClientTLS(host, port, username, password)
}

// NewClientTLS returns a new (not connected) Client, using TLS.
func NewClientTLS(host string, port int, username, password string) Client {
	if port == 0 {
		port = 143
	}
	return &client{host: host, port: port, username: username, password: password, tls: forceTLS}
}

// NewClientNoTLS returns a new (not connected) Client, without TLS.
func NewClientNoTLS(host string, port int, username, password string) Client {
	if port == 0 {
		port = 143
	}
	return &client{host: host, port: port, username: username, password: password, tls: noTLS}
}

// String returns the connection parameters.
func (c client) String() string {
	return c.username + "@" + c.host + ":" + strconv.Itoa(c.port)
}

// SetLogMask allows setting the underlying imap.LogMask.
func (c client) SetLogMask(mask imap.LogMask) imap.LogMask {
	return c.c.SetLogMask(imap.LogAll)
}

// ReadTo reads the message identified by the given msgID, into the io.Writer.
func (c client) ReadTo(w io.Writer, msgID uint32) (int64, error) {
	var length int64
	set := &imap.SeqSet{}
	set.AddNum(msgID)

	cmd, err := c.c.UIDFetch(set, "BODY.PEEK[]")
	if err != nil {
		return length, err
	}

	for cmd.InProgress() {
		// wait for server response
		if err = c.c.Recv(Timeout); err != nil {
			if err == io.EOF {
				break
			}
			return length, err
		}
		// Process data.
		for _, resp := range cmd.Data {
			//Log.Debug("resp", "resp", resp, "messageinfo", resp.MessageInfo(), "attrs", resp.MessageInfo().Attrs)
			n, err := w.Write(imap.AsBytes(resp.MessageInfo().Attrs["BODY[]"]))
			if err != nil {
				return length, err
			}
			length += int64(n)
		}
		cmd.Data = nil
	}

	// Check command completion status.
	if _, err = cmd.Result(imap.OK); err != nil {
		return length, err
	}
	return length, nil
}

// Move the msgID to the given mbox.
func (c *client) Move(msgID uint32, mbox string) error {
	created := false
	for _, k := range c.created {
		if mbox == k {
			created = true
			break
		}
	}
	if !created {
		Log.Info("Create", "mbox", mbox)
		c.created = append(c.created, mbox)
		if _, err := imap.Wait(c.c.Create(mbox)); err != nil {
			Log.Error("Create", "mbox", mbox, "error", err)
		}
	}

	set := &imap.SeqSet{}
	set.AddNum(msgID)

	if _, err := imap.Wait(c.c.UIDCopy(set, mbox)); err != nil {
		return err
	}

	return c.MarkDeleted(msgID)
}

// Get the Flags by MsgId.
func (c *client) SetFlagRegex(msgID uint32, regex string, st bool) error {
	flags, err := c.GetFlags(msgID)
	if err != nil {
		return err
	}

	rex := regexp.MustCompile(regex)
	for flag := range flags {
		if rex.MatchString(flag) {
			if err := c.SetFlag(msgID, flag, st); err != nil {
				return err
			}
		}
	}

	return nil
}

// Get the Flags by MsgId.
func (c *client) GetFlags(msgID uint32) (imap.FlagSet, error) {
	set := &imap.SeqSet{}
	set.AddNum(msgID)

	cmd, err := imap.Wait(c.c.UIDFetch(set, "FLAGS"))
	if err != nil {
		return nil, err
	}

	if _, err = cmd.Result(imap.OK); err != nil {
		return nil, err
	}

	resp := cmd.Data[0].MessageInfo()
	return resp.Flags, nil
}

// List the messages from the given mbox, matching the pattern.
// Lists only new (UNSEEN) messages iff all is false.
func (c *client) List(mbox, pattern string, all bool) ([]uint32, error) {
	Log.Debug("List", "mbox", mbox, "pattern", pattern)
	_, err := imap.Wait(c.c.Select(mbox, false))
	if err != nil {
		return nil, err
	}
	var fields = make([]imap.Field, 0, 4)
	if all {
		fields = append(fields, imap.Field("NOT"), imap.Field("DELETED"))
	} else {
		fields = append(fields, imap.Field("UNSEEN"))
	}
	if pattern != "" {
		fields = append(fields, imap.Field("SUBJECT"), c.c.Quote(pattern))
	}
	ok := false
	var cmd *imap.Command
	if !c.noUTF8 {
		if cmd, err = imap.Wait(c.c.UIDSearch(fields...)); err != nil {
			Log.Debug("UIDSearch", "fields", fields, "error", err)
			if strings.Index(err.Error(), "BADCHARSET") >= 0 {
				c.noUTF8 = true
			} else {
				return nil, err
			}
		} else {
			ok = true
		}
	}
	if !ok && c.noUTF8 {
		if pattern != "" {
			fields[len(fields)-1] = c.c.Quote(imap.UTF7Encode(pattern))
		}
		cmd, err = imap.Wait(c.c.Send("UID SEARCH", fields))
		Log.Debug("UID SEARCH", "fields", fields, "error", err)
		if err != nil {
			return nil, err
		}
	}
	if _, err = cmd.Result(imap.OK); err != nil {
		return nil, err
	}
	Log.Debug("List", "data", cmd.Data)
	var uids []uint32
	for _, resp := range cmd.Data {
		uids = append(uids, resp.SearchResults()...)
	}
	return uids, nil
}

// Close closes the currently selected mailbox, then logs out.
func (c *client) Close(expunge bool) error {
	if c.c == nil {
		return nil
	}
	c.c.Close(expunge)
	_, err := imap.Wait(c.c.Logout(Timeout))
	c.c = nil
	return err
}

// Mark the message seen
func (c *client) MarkSeen(msgID uint32) error {
	return c.SetFlag(msgID, `\Seen`, true)
}

// Mark the message unseen
func (c *client) MarkUnseen(msgID uint32) error {
	return c.SetFlag(msgID, `\Seen`, false)
}

// Mark the message deleted
func (c *client) MarkDeleted(msgID uint32) error {
	return c.SetFlag(msgID, `\Deleted`, true)
}

// Mark the message undeleted
func (c *client) MarkUndeleted(msgID uint32) error {
	return c.SetFlag(msgID, `\Deleted`, false)
}

// Set the specified keyword
func (c *client) SetFlag(msgID uint32, keyword string, st bool) error {
	set := &imap.SeqSet{}
	set.AddNum(msgID)

	item := "+FLAGS"
	if !st {
		item = "-FLAGS"
	}
	_, err := imap.Wait(c.c.UIDStore(set, item, imap.Field(keyword)))
	return err
}

// Connect to the server.
func (c *client) Connect() error {
	addr := c.host + ":" + strconv.Itoa(c.port)
	var err error
	if c.tls == noTLS || c.tls == maybeTLS && c.port == 143 {
		c.c, err = imap.Dial(addr)
	} else {
		c.c, err = imap.DialTLS(addr, &TLSConfig)
	}
	if err != nil {
		return err
	}
	c.c.SetLogger(loghlp.AsStdLog(Log, log15.LvlDebug))
	// Print server greeting (first response in the unilateral server data queue)
	Log.Debug("Server says", "hello", c.c.Data[0].Info)
	c.c.Data = nil

	Log.Debug("server", "capabilities", c.c.Caps)
	// Enable encryption, if supported by the server
	if c.c.Caps["STARTTLS"] {
		c.c.StartTLS(nil)
	}

	// Authenticate
	if c.c.State() == imap.Login {
		if _, err = c.c.Login(c.username, c.password); err != nil {
			Log.Error("Login", "username", c.username, "capabilities", c.c.Caps, "error", err)
			if _, err = c.c.Auth(CramAuth(c.username, c.password)); err != nil {
				Log.Error("Authenticate", "username", c.username, "capabilities", c.c.Caps, "error", err)
				return err
			}
		}
	}

	if _, err := c.c.CompressDeflate(2); err != nil {
		Log.Info("CompressDeflate", "error", err)
	}

	return nil
}
