package awfulirc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

type Command int

const (
	Command_Unknown Command = iota
	Command_Pass
	Command_Nick
	Command_User
	Command_Server
	Command_Oper
	Command_Quit
	Command_Squit
	Command_Join
	Command_Part
	Command_Mode
	Command_Topic
	Command_Names
	Command_List
	Command_Invite
	Command_Kick
	Command_Version
	Command_Stats
	Command_Links
	Command_Time
	Command_Connect
	Command_Trace
	Command_Admin
	Command_Info
	Command_Privmsg
	Command_Notice
	Command_Who
	Command_Whois
	Command_Whowas
	Command_Kill
	Command_Ping
	Command_Pong
	Command_Error
	Command_Cap
	Command_Ison
)

var (
	ErrMessageTooLong    = errors.New("message too long")
	ErrPrefixOnlyMessage = errors.New("message only contains prefix")
	ErrEmptyCommand      = errors.New("message does not contain command")
)

// ClientMessage is a parsed message from the connected client.
type ClientMessage struct {
	// Prefix is the optional message prefix. The colon prefix is not
	// included in this string.
	Prefix string

	// RawCommand is the command provided by the client.
	RawCommand string

	// Command is the parsed command from the client, or
	// Command_Unknown if it cannot be parsed.
	Command

	// Parameters contains all command parameters, including the
	// trailing parameter as the last element of the slice.
	Parameters []string
}

// NewClientMessageIterator iterates the raw client stream and returns
// a sequence of parsed IRC commands.
func NewClientMessageIterator(r io.Reader) func(yield func(*ClientMessage, error) bool) {
	return func(yield func(*ClientMessage, error) bool) {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 512), 0)
		s.Split(splitCRLF)

		for s.Scan() {
			buf := s.Bytes()
			var (
				msg *ClientMessage
				err error
			)

			switch {
			case len(buf) == 0:
				continue
			case len(buf) > 512:
				err = ErrMessageTooLong
			default:
				msg, err = ParseSingleClientMessage(buf)
			}

			if ok := yield(msg, err); !ok {
				return
			}
		}
		if err := s.Err(); err != nil {
			yield(nil, err)
		}
	}
}

// ParseSingleClientMessage parses a message from a single IRC
// line. The CR LF delimiter must have been removed.
func ParseSingleClientMessage(msg []byte) (*ClientMessage, error) {
	var (
		prefix      string
		inPrefix    bool
		startPrefix int

		command      string
		inCommand    bool
		startCommand int

		args []string

		inStandardArg bool
		inTrailingArg bool
		startArg      int
	)

ForEachByte:
	for i, b := range msg {
		switch {
		case i == 0 && b == ':':
			inPrefix = true
			startPrefix = i + 1
		case i == 0:
			inCommand = true
			startCommand = 0
		case inPrefix && b == ' ':
			inPrefix = false
			inCommand = true
			startCommand = i + 1
			prefix = string(msg[startPrefix:i])
		case inPrefix:
			// Simply advance the index and accumulate the prefix.
		case inCommand && b == ' ':
			inCommand = false
			command = string(msg[startCommand:i])
		case inCommand:
			// Simply advance the index and accumulate the command.
		case inStandardArg && b == ' ':
			inStandardArg = false
			args = append(args, string(msg[startArg:i]))
		case inStandardArg:
			// Simply advance the index and accumulate the argument.
		case b == ' ':
			// Skip spaces when not in an existing context.
		case b == ':':
			// Must be trailing.
			inTrailingArg = true
			startArg = i + 1
			break ForEachByte
		default:
			// Must start a new argument.
			inStandardArg = true
			startArg = i
		}
	}

	switch {
	case inPrefix:
		return nil, ErrPrefixOnlyMessage
	case inCommand && startCommand < len(msg):
		command = string(msg[startCommand:])
	case inCommand, command == "":
		return nil, ErrEmptyCommand
	case inStandardArg:
		args = append(args, string(msg[startArg:]))
	case inTrailingArg && startArg < len(msg):
		args = append(args, string(msg[startArg:]))
	case inTrailingArg:
		args = append(args, "") // Allow empty string as a special case
	}

	cm := &ClientMessage{
		Prefix:     prefix,
		RawCommand: command,
		Parameters: args,
	}

	switch strings.ToLower(command) {
	case "pass":
		cm.Command = Command_Pass
	case "nick":
		cm.Command = Command_Nick
	case "user":
		cm.Command = Command_User
	case "server":
		cm.Command = Command_Server
	case "oper":
		cm.Command = Command_Oper
	case "quit":
		cm.Command = Command_Quit
	case "squit":
		cm.Command = Command_Squit
	case "join":
		cm.Command = Command_Join
	case "part":
		cm.Command = Command_Part
	case "mode":
		cm.Command = Command_Mode
	case "topic":
		cm.Command = Command_Topic
	case "names":
		cm.Command = Command_Names
	case "list":
		cm.Command = Command_List
	case "invite":
		cm.Command = Command_Invite
	case "kick":
		cm.Command = Command_Kick
	case "version":
		cm.Command = Command_Version
	case "stats":
		cm.Command = Command_Stats
	case "links":
		cm.Command = Command_Links
	case "time":
		cm.Command = Command_Time
	case "connect":
		cm.Command = Command_Connect
	case "trace":
		cm.Command = Command_Trace
	case "admin":
		cm.Command = Command_Admin
	case "info":
		cm.Command = Command_Info
	case "privmsg":
		cm.Command = Command_Privmsg
	case "notice":
		cm.Command = Command_Notice
	case "who":
		cm.Command = Command_Who
	case "whois":
		cm.Command = Command_Whois
	case "whowas":
		cm.Command = Command_Whowas
	case "kill":
		cm.Command = Command_Kill
	case "ping":
		cm.Command = Command_Ping
	case "pong":
		cm.Command = Command_Pong
	case "error":
		cm.Command = Command_Error
	case "cap":
		cm.Command = Command_Cap
	case "ison":
		cm.Command = Command_Ison
	}

	return cm, nil
}

func splitCRLF(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	// \r\n means we have a full command.
	i := bytes.IndexByte(data, '\r')
	if i >= 0 && i+1 < len(data) && data[i+1] == '\n' {
		return i + 2, data[0:i], nil
	}

	// If at end of file with data return the data.
	if atEOF {
		return len(data), data, nil
	}

	// Request more data
	return 0, nil, nil
}
