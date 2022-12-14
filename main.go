package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
	"github.com/go-logfmt/logfmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/typ.v4/slices"
)

var parsedTime time.Time

func main() {
	loggerSetup()
	relogger := NewRelogger(os.Stdin)

	if err := relogger.RelogAll(); err != nil {
		log.Err(err).Msg("Failed to scan.")
	}
}

func NewRelogger(r io.Reader) Relogger {
	return Relogger{
		scanner: bufio.NewScanner(os.Stdin),
	}
}

type Relogger struct {
	scanner *bufio.Scanner

	mongoCompWidth    int
	mongoContextWidth int
	mongoIDWidth      int

	buf bytes.Buffer
}

func (r *Relogger) RelogAll() error {
	for r.scanner.Scan() {
		r.processLine(r.scanner.Bytes())
	}
	return r.scanner.Err()
}

func (r *Relogger) processLine(b []byte) {
	if r.processLineJson(b) {
		return
	}
	if r.processLineLogFmt(b) {
		return
	}
	log.WithLevel(zerolog.NoLevel).Msg(string(b))
}

func (r *Relogger) processLineJson(b []byte) bool {
	if r.buf.Len() > 0 {
		r.buf.Write(b)
		b = r.buf.Bytes()
	}
	root, err := sonic.Get(b)
	if err != nil {
		if isSonicEOFErr(err) {
			if r.buf.Len() == 0 {
				r.buf.Write(b)
			}
			return true
		}
		r.buf.Reset()
		return false
	}
	r.buf.Reset()
	if root.Type() != ast.V_OBJECT {
		return false
	}
	var (
		level            = zerolog.NoLevel
		message          = ""
		isMongoDBLogging = false
		ignoreNodes      []string
	)
	levelNodeName, levelNode := findWithAnyName(root, "level", "lvl", "severity")
	if levelNode != nil {
		if levelStr, err := levelNode.String(); err == nil {
			level = parseLevel(levelStr)
			ignoreNodes = append(ignoreNodes, levelNodeName)
		}
	} else {
		// MongoDB styled logging
		// https://www.mongodb.com/docs/manual/reference/log-messages/#std-label-log-severity-levels
		levelNodeName, levelNode = findWithAnyName(root, "s")
		if levelNode != nil {
			if levelStr, err := levelNode.String(); err == nil {
				if lvl, ok := parseMongoDBLevel(levelStr); ok {
					level = lvl
					isMongoDBLogging = true
					ignoreNodes = append(ignoreNodes, levelNodeName)
				}
			}
		}
	}

	messageNodeName, messageNode := findWithAnyName(root, "message", "msg")
	if messageNode != nil {
		if messageStr, err := messageNode.String(); err == nil {
			message = messageStr
			ignoreNodes = append(ignoreNodes, messageNodeName)

			if isMongoDBLogging {
				_, componentNode := findWithAnyName(root, "c")
				_, contextNode := findWithAnyName(root, "ctx")
				_, idNode := findWithAnyName(root, "id")
				if componentNode != nil && contextNode != nil && idNode != nil {
					componentStr, _ := componentNode.String()
					contextStr, _ := contextNode.String()
					idStr, _ := idNode.String()

					r.mongoCompWidth, componentStr = padString(r.mongoCompWidth, componentStr)
					r.mongoContextWidth, contextStr = padString(r.mongoContextWidth, contextStr)
					r.mongoIDWidth, idStr = padString(r.mongoIDWidth, idStr)

					message = fmt.Sprintf("[%s|%s|%s] %s", componentStr, contextStr, idStr, message)
				} else {
					isMongoDBLogging = false
				}
			}
		}
	}

	timestampNodeName, timestampNode := findWithAnyName(root, "time", "timestamp", "@timestamp", "ts", "datetime")
	if t, ok := parseTimestampNode(timestampNode); ok {
		parsedTime = t
		ignoreNodes = append(ignoreNodes, timestampNodeName)
	} else if isMongoDBLogging {
		timestampNode = root.GetByPath("t", "$date")
		if t, ok := parseTimestampNode(timestampNode); ok {
			parsedTime = t
		}
	}

	if isMongoDBLogging {
		attrNode := root.Get("attr")
		if attrNode != nil {
			root = *attrNode
		}
	}

	stacktraceNodeName, stacktraceNode := findWithAnyName(root, "stacktrace", "stack_trace", "stack")
	if stacktraceNode != nil {
		ignoreNodes = append(ignoreNodes, stacktraceNodeName)
		if stacktraceNode.Type() == ast.V_ARRAY {
			children, _ := stacktraceNode.ArrayUseNode()
			var sb strings.Builder
			for _, child := range children {
				childStr, _ := child.String()
				sb.WriteString(childStr)
				sb.WriteString("\n\t")
			}
			message = fmt.Sprintf("%s\n\tSTACKTRACE\n\t==========\n\t%s", message, sb.String())
		} else {
			stacktraceStr, _ := stacktraceNode.String()
			stacktraceStr = strings.ReplaceAll(stacktraceStr, "\n", "\n\t")
			message = fmt.Sprintf("%s\n\tSTACKTRACE\n\t==========\n\t%s", message, stacktraceStr)
		}
	}

	ev := log.WithLevel(level)

	root.ForEach(func(path ast.Sequence, node *ast.Node) bool {
		if path.Key == nil {
			return true
		}
		key := *path.Key
		if slices.Contains(ignoreNodes, key) {
			return true // skip, already processed
		}
		switch node.Type() {
		case ast.V_NULL:
			ev = ev.Interface(key, nil)
		case ast.V_TRUE:
			ev = ev.Bool(key, true)
		case ast.V_FALSE:
			ev = ev.Bool(key, false)
		case ast.V_ARRAY:
			arr, _ := node.Array()
			ev = ev.Interface(key, arr)
		case ast.V_OBJECT:
			m, _ := node.Map()
			ev = ev.Interface(key, m)
		case ast.V_STRING:
			str, _ := node.String()
			ev = ev.Str(key, str)
		case ast.V_NUMBER:
			num, _ := node.Number()
			if i, err := strconv.ParseInt(num.String(), 10, 64); err == nil {
				ev = ev.Int64(key, i)
			} else if f, err := strconv.ParseFloat(num.String(), 64); err == nil {
				ev = ev.Float64(key, f)
			} else {
				ev = ev.Str(key, num.String())
			}
		}
		return true
	})
	ev.Msg(message)
	return true
}

func (r Relogger) processLineLogFmt(b []byte) bool {
	d := logfmt.NewDecoder(bytes.NewReader(b))
	if !d.ScanRecord() {
		return false
	}
	var (
		timestamp    time.Time
		hasTimestamp bool
		level        = zerolog.NoLevel
		hasLevel     bool
		message      string
		hasMessage   bool
	)
	type Pair struct {
		Key   string
		Value string
	}
	var fields []Pair
	for d.ScanKeyval() {
		pair := Pair{string(d.Key()), string(d.Value())}
		if !hasTimestamp && (pair.Key == "time" || pair.Key == "timestamp" || pair.Key == "@timestamp" || pair.Key == "ts" || pair.Key == "datetime") {
			if t, ok := parseTime(pair.Value); ok {
				timestamp = t
				hasTimestamp = true
				continue
			}
		} else if !hasLevel && (pair.Key == "level" || pair.Key == "lvl" || pair.Key == "severity") {
			level = parseLevel(pair.Value)
			hasLevel = true
			continue
		} else if !hasMessage && (pair.Key == "message" || pair.Key == "msg") {
			message = pair.Value
			hasMessage = true
			continue
		}
		fields = append(fields, pair)
	}
	if hasTimestamp {
		parsedTime = timestamp
	} else {
		parsedTime = time.Time{}
	}
	ev := log.WithLevel(level)
	for _, pair := range fields {
		if i, err := strconv.ParseInt(pair.Value, 10, 64); err == nil {
			ev = ev.Int64(pair.Key, i)
		} else if f, err := strconv.ParseFloat(pair.Value, 64); err == nil {
			ev = ev.Float64(pair.Key, f)
		} else if pair.Value == "true" {
			ev = ev.Bool(pair.Key, true)
		} else if pair.Value == "false" {
			ev = ev.Bool(pair.Key, false)
		} else {
			ev = ev.Str(pair.Key, pair.Value)
		}
	}
	if len(fields) == 0 && !hasMessage && !hasLevel && !hasTimestamp {
		return false
	}
	ev.Msg(message)
	return true
}

func findWithAnyName(node ast.Node, names ...string) (string, *ast.Node) {
	var name string
	var child *ast.Node
	node.ForEach(func(path ast.Sequence, node *ast.Node) bool {
		if path.Key == nil {
			return false
		}
		key := *path.Key
		for _, n := range names {
			if key == n {
				name = n
				child = node
				return false
			}
		}
		return true
	})
	return name, child
}

func parseLevel(levelStr string) zerolog.Level {
	level, err := zerolog.ParseLevel(strings.ToLower(levelStr))
	if err != nil {
		return zerolog.NoLevel
	}
	return level
}

func parseMongoDBLevel(levelStr string) (zerolog.Level, bool) {
	switch levelStr {
	case "F":
		return zerolog.FatalLevel, true
	case "E":
		return zerolog.ErrorLevel, true
	case "W":
		return zerolog.WarnLevel, true
	case "I":
		return zerolog.InfoLevel, true
	case "D1":
		return zerolog.DebugLevel, true
	case "D2", "D3", "D4", "D5":
		return zerolog.TraceLevel, true
	default:
		return zerolog.NoLevel, false
	}
}

func parseTimestampNode(node *ast.Node) (time.Time, bool) {
	if node == nil {
		return time.Time{}, false
	}
	if node.Type() == ast.V_NUMBER {
		if i, err := node.Int64(); err != nil {
			return time.Unix(i, 0), true
		}
	} else if timestampStr, err := node.String(); err == nil {
		return parseTime(timestampStr)
	}
	return time.Time{}, false
}

var knownTimestampLayouts = []string{
	time.RFC3339,
	time.RFC3339Nano,
}

func parseTime(str string) (time.Time, bool) {
	for _, layout := range knownTimestampLayouts {
		if t, err := time.Parse(layout, str); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var eofErrRegex = regexp.MustCompile(`^"Syntax error at index \d+: eof`)

func isSonicEOFErr(err error) bool {
	return eofErrRegex.MatchString(err.Error())
}

func padString(prevMax int, str string) (int, string) {
	if len(str) == prevMax {
		return prevMax, str
	}
	if len(str) > prevMax {
		return len(str), str
	}
	return prevMax, str + strings.Repeat(" ", prevMax-len(str))
}

func loggerSetup() error {
	zerolog.TimestampFunc = func() time.Time {
		return parsedTime
	}
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "Jan-02 15:04",
	}).Level(zerolog.TraceLevel)
	return nil
}
