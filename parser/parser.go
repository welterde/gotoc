package parser

import (
	"fmt"
	"io/ioutil"
	//"log"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"

	. "goprotobuf.googlecode.com/hg/compiler/descriptor"
	"goprotobuf.googlecode.com/hg/proto"
)

func ParseFiles(filenames []string, importPaths []string) (*FileDescriptorSet, os.Error) {
	fds := &FileDescriptorSet{
		File: make([]*FileDescriptorProto, 0, len(filenames)),
	}

	parsedFiles := make(map[string]int, len(filenames))
	for len(filenames) > 0 {
		filename := filenames[0]
		filenames = filenames[1:]
		if _, ok := parsedFiles[filename]; ok {
			continue
		}
		fd := &FileDescriptorProto{
			Name: proto.String(filename),
		}
		fds.File = append(fds.File, fd)

		fullFilename := resolveFilename(filename, importPaths)
		if fullFilename == "" {
			return nil, fmt.Errorf("failed finding %q", filename)
		}
		buf, err := ioutil.ReadFile(fullFilename)
		if err != nil {
			return nil, err
		}

		p := newParser(string(buf))
		if pe := p.readFile(fd); pe != nil {
			return nil, pe
		}
		if p.s != "" {
			return nil, p.error("input was not all consumed")
		}

		// Enqueue dependencies.
		for _, f := range fd.Dependency {
			if _, ok := parsedFiles[f]; !ok {
				filenames = append(filenames, f)
			}
		}
	}

	return fds, nil
}

// TODO: This is almost identical to fullPath in main.go. Merge them.
func resolveFilename(filename string, paths []string) string {
	for _, p := range paths {
		full := path.Join(p, filename)
		fi, err := os.Stat(full)
		if err == nil && fi.IsRegular() {
			return full
		}
	}
	return ""
}

type parseError struct {
	message string
	line    int // 1-based line number
	offset  int // 0-based byte offset from start of input
}

func (pe *parseError) String() string {
	if pe == nil {
		return "<nil>"
	}
	if pe.line == 1 {
		return fmt.Sprintf("line 1.%d: %v", pe.offset, pe.message)
	}
	return fmt.Sprintf("line %d: %v", pe.line, pe.message)
}

type token struct {
	value        string
	err          *parseError
	line, offset int
	unquoted     string // unquoted version of value
}

type parser struct {
	s            string // remaining input
	done         bool   // whether the parsing is finished
	backed       bool   // whether back() was called
	offset, line int
	cur          token
}

func newParser(s string) *parser {
	return &parser{
		s:    s,
		line: 1,
		cur: token{
			line: 1,
		},
	}
}

func (p *parser) readFile(fd *FileDescriptorProto) *parseError {
	// Parse the top-level things.
	for !p.done {
		tok := p.next()
		if tok.err != nil {
			return tok.err
		}
		switch tok.value {
		case "package":
			parts := make([]string, 0, 3) // enough for most
			for {
				tok := p.next()
				if tok.err != nil {
					return tok.err
				}
				more := false
				if tok.value[len(tok.value)-1] == '.' {
					tok.value = tok.value[:len(tok.value)-1]
					more = true
				}
				parts = append(parts, tok.value)
				if more {
					continue
				}

				// If a period is the next token then there's another package component.
				tok = p.next()
				if tok.err != nil {
					return tok.err
				}
				if tok.value != "." {
					p.back()
					break
				}
			}
			// TODO: check for a good package name
			fd.Package = proto.String(strings.Join(parts, "."))

			if err := p.readToken(";"); err != nil {
				return err
			}
		case "import":
			tok, err := p.readString()
			if err != nil {
				return err
			}
			fd.Dependency = append(fd.Dependency, tok.unquoted)

			if err := p.readToken(";"); err != nil {
				return err
			}
		case "enum":
			p.back()
			e := new(EnumDescriptorProto)
			fd.EnumType = append(fd.EnumType, e)
			if err := p.readEnum(e); err != nil {
				return err
			}
		case "message":
			p.back()
			msg := new(DescriptorProto)
			fd.MessageType = append(fd.MessageType, msg)
			if err := p.readMessage(msg); err != nil {
				return err
			}
		// TODO: more top-level things
		case "":
			// EOF
			break
		default:
			return p.error("unknown top-level thing %q", tok.value)
		}
	}

	// TODO: more

	return nil
}

func (p *parser) readEnum(e *EnumDescriptorProto) *parseError {
	if err := p.readToken("enum"); err != nil {
		return err
	}

	tok := p.next()
	if tok.err != nil {
		return tok.err
	}
	// TODO: check that the name is acceptable.
	e.Name = proto.String(tok.value)

	if err := p.readToken("{"); err != nil {
		return err
	}

	// Parse enum values
	for !p.done {
		tok := p.next()
		if tok.err != nil {
			return tok.err
		}
		if tok.value == "}" {
			// end of enum
			return nil
		}
		// TODO: verify tok.value is a valid enum value name.
		ev := new(EnumValueDescriptorProto)
		e.Value = append(e.Value, ev)
		ev.Name = proto.String(tok.value)

		if err := p.readToken("="); err != nil {
			return err
		}

		tok = p.next()
		if tok.err != nil {
			return tok.err
		}
		// TODO: check that tok.value is a valid enum value number.
		num, err := atoi32(tok.value)
		if err != nil {
			return p.error("bad enum number %q: %v", tok.value, err)
		}
		ev.Number = proto.Int32(num)

		if err := p.readToken(";"); err != nil {
			return err
		}
	}

	return p.error("unexpected end while parsing enum")
}

func (p *parser) readMessage(d *DescriptorProto) *parseError {
	if err := p.readToken("message"); err != nil {
		return err
	}

	tok := p.next()
	if tok.err != nil {
		return tok.err
	}
	// TODO: check that the name is acceptable.
	d.Name = proto.String(tok.value)

	if err := p.readToken("{"); err != nil {
		return err
	}

	// Parse message fields and other things inside messages.
	for !p.done {
		tok := p.next()
		if tok.err != nil {
			return tok.err
		}
		switch tok.value {
		case "required", "optional", "repeated":
			// field
			p.back()
			f := new(FieldDescriptorProto)
			d.Field = append(d.Field, f)
			if err := p.readField(f); err != nil {
				return err
			}
		case "enum":
			// nested enum
			p.back()
			e := new(EnumDescriptorProto)
			d.EnumType = append(d.EnumType, e)
			if err := p.readEnum(e); err != nil {
				return err
			}
		case "message":
			// nested message
			p.back()
			msg := new(DescriptorProto)
			d.NestedType = append(d.NestedType, msg)
			if err := p.readMessage(msg); err != nil {
				return err
			}
		// TODO: more message contents
		case "}":
			// end of message
			return nil
		default:
			return p.error("unexpected token %q while parsing message", tok.value)
		}
	}

	return p.error("unexpected end while parsing message")
}

var fieldLabelMap = map[string]*FieldDescriptorProto_Label{
	"required": NewFieldDescriptorProto_Label(FieldDescriptorProto_LABEL_REQUIRED),
	"optional": NewFieldDescriptorProto_Label(FieldDescriptorProto_LABEL_OPTIONAL),
	"repeated": NewFieldDescriptorProto_Label(FieldDescriptorProto_LABEL_REPEATED),
}

var fieldTypeMap = map[string]*FieldDescriptorProto_Type{
	// Only basic types; enum, message and group are handled differently.
	"double":   NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_DOUBLE),
	"float":    NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_FLOAT),
	"int64":    NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_INT64),
	"uint64":   NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_UINT64),
	"int32":    NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_INT32),
	"fixed64":  NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_FIXED64),
	"fixed32":  NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_FIXED32),
	"bool":     NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_BOOL),
	"string":   NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_STRING),
	"bytes":    NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_BYTES),
	"uint32":   NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_UINT32),
	"sfixed32": NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_SFIXED32),
	"sfixed64": NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_SFIXED64),
	"sint32":   NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_SINT32),
	"sint64":   NewFieldDescriptorProto_Type(FieldDescriptorProto_TYPE_SINT64),
}

func (p *parser) readField(f *FieldDescriptorProto) *parseError {
	tok := p.next()
	if tok.err != nil {
		return tok.err
	}
	if lab, ok := fieldLabelMap[tok.value]; ok {
		f.Label = lab
	} else {
		return p.error("expected required/optional/repeated, found %q", tok.value)
	}

	tok = p.next()
	if tok.err != nil {
		return tok.err
	}
	if typ, ok := fieldTypeMap[tok.value]; ok {
		f.Type = typ
	} else {
		f.TypeName = proto.String(tok.value)
	}

	tok = p.next()
	if tok.err != nil {
		return tok.err
	}
	// TODO: check field name correctness (character set, etc.)
	f.Name = proto.String(tok.value)

	if err := p.readToken("="); err != nil {
		return err
	}

	f.Number = new(int32)
	if err := p.readTagNumber(f.Number); err != nil {
		return err
	}

	// TODO: default value, options

	if err := p.readToken(";"); err != nil {
		return err
	}

	return nil
}

func (p *parser) readTagNumber(num *int32) *parseError {
	tok := p.next()
	if tok.err != nil {
		return tok.err
	}
	n, err := atoi32(tok.value)
	if err != nil {
		return p.error("bad field number %q: %v", tok.value, err)
	}
	if n < 1 || n >= (1 << 29) {
		return p.error("field number %v out of range", n)
	}
	// 19000-19999 are reserved.
	if n >= 19000 && n <= 19999 {
		return p.error("field number %v in reserved range [19000, 19999]", n)
	}
	*num = n
	return nil
}

func (p *parser) readString() (*token, *parseError) {
	tok := p.next()
	if tok.err != nil {
		return nil, tok.err
	}
	if tok.value[0] != '"' {
		return nil, p.error("expected string, found %q", tok.value)
	}
	return tok, nil
}

func (p *parser) readToken(expected string) *parseError {
	tok := p.next()
	if tok.err != nil {
		return tok.err
	}
	if tok.value != expected {
		return p.error("expected %q, found %q", expected, tok.value)
	}
	return nil
}

// Back off the parser by one token; may only be done between calls to p.next().
func (p *parser) back() {
	p.backed = true
}

// Advances the parser and returns the new current token.
func (p *parser) next() *token {
	if p.backed || p.done {
		p.backed = false
	} else {
		p.advance()
		if p.done {
			p.cur.value = ""
		}
	}
	//log.Printf("parser·next(): returning %q [err: %v]", p.cur.value, p.cur.err)
	return &p.cur
}

func (p *parser) advance() {
	// Skip whitespace
	p.skipWhitespaceAndComments()
	if p.done {
		return
	}

	// Start of non-whitespace
	p.cur.err = nil
	p.cur.offset, p.cur.line = p.offset, p.line
	switch p.s[0] {
	// TODO: more cases, like punctuation.
	case ';', '{', '}', '=':
		// Single symbol
		p.cur.value, p.s = p.s[:1], p.s[1:]
	case '"':
		// Quoted string
		i := 1
		for i < len(p.s) && p.s[i] != '"' {
			if p.s[i] == '\\' && i+1 < len(p.s) {
				// skip escaped character
				i++
			}
			i++
		}
		if i >= len(p.s) {
			p.error("encountered EOF inside string")
			return
		}
		i++
		p.cur.value, p.s = p.s[:i], p.s[i:]
		unq, err := strconv.Unquote(p.cur.value)
		if err != nil {
			p.error("invalid quoted string: %v", err)
		}
		p.cur.unquoted = unq
	default:
		i := 0
		for i < len(p.s) && isIdentOrNumberChar(p.s[i]) {
			i++
		}
		if i == 0 {
			p.error("unexpected byte 0x%02x (%q)", p.s[0], string(p.s[:1]))
			return
		}
		p.cur.value, p.s = p.s[:i], p.s[i:]
	}
	p.offset += len(p.cur.value)
}

func (p *parser) skipWhitespaceAndComments() {
	i := 0
	for i < len(p.s) {
		if isWhitespace(p.s[i]) {
			if p.s[i] == '\n' {
				p.line++
			}
			i++
			continue
		}
		if i+1 < len(p.s) && p.s[i] == '/' && p.s[i+1] == '/' {
			// comment; skip to end of line or input
			for i < len(p.s) && p.s[i] != '\n' {
				i++
			}
			if i < len(p.s) {
				// end of line; keep going
				p.line++
				i++
				continue
			}
			// end of input; fall out of loop
		}
		break
	}
	p.offset += i
	p.s = p.s[i:]
	if len(p.s) == 0 {
		p.done = true
	}
}

func (p *parser) error(format string, a ...interface{}) *parseError {
	pe := &parseError{
		message: fmt.Sprintf(format, a...),
		line:    p.cur.line,
		offset:  p.cur.offset,
	}
	p.cur.err = pe
	p.done = true
	return pe
}

func isWhitespace(c byte) bool {
	// TODO: do more accurately
	return unicode.IsSpace(int(c))
}

// Numbers and identifiers are matched by [-+._A-Za-z0-9]
func isIdentOrNumberChar(c byte) bool {
	switch {
	case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z':
		return true
	case '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '-', '+', '.', '_':
		return true
	}
	return false
}

func atoi32(s string) (int32, os.Error) {
	x, err := strconv.Atoi64(s)
	if err != nil {
		return 0, err
	}
	if x < (-1 << 31) || x > (1<<31 - 1) {
		return 0, os.NewError("out of int32 range")
	}
	return int32(x), nil
}
