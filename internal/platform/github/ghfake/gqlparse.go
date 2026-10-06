package ghfake

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A small GraphQL front end: a lexer, a parser of executable documents
// (operations, selections, arguments, variables, inline fragments and
// fragment spreads) and of the schema definition language the fake's
// schema subset is written in. It follows the GraphQL specification
// (October 2021) for everything it accepts.

// tokKind is a token kind.
type tokKind uint8

const (
	tokEOF tokKind = iota
	tokPunct
	tokName
	tokInt
	tokFloat
	tokString
)

// gqlTok is one lexical token with its position (1-based).
type gqlTok struct {
	kind      tokKind
	val       string
	line, col int
}

// lexer splits a GraphQL source into tokens.
type lexer struct {
	src       string
	pos       int
	line, col int
}

// gqlSyntaxError is a parse error at a position.
type gqlSyntaxError struct {
	msg       string
	line, col int
}

func (e *gqlSyntaxError) Error() string { return e.msg }

// next returns the next token.
func (l *lexer) next() (gqlTok, error) {
	l.skip()
	if l.pos >= len(l.src) {
		return gqlTok{kind: tokEOF, line: l.line, col: l.col}, nil
	}
	start := gqlTok{line: l.line, col: l.col}
	c := l.src[l.pos]
	switch {
	case strings.HasPrefix(l.src[l.pos:], "..."):
		l.advance(3)
		start.kind, start.val = tokPunct, "..."
		return start, nil
	case strings.IndexByte("!$&()://=@[]{|}", c) >= 0:
		l.advance(1)
		start.kind, start.val = tokPunct, string(c)
		return start, nil
	case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		i := l.pos
		for i < len(l.src) && (l.src[i] == '_' || l.src[i] >= 'a' && l.src[i] <= 'z' || l.src[i] >= 'A' && l.src[i] <= 'Z' || l.src[i] >= '0' && l.src[i] <= '9') {
			i++
		}
		start.kind, start.val = tokName, l.src[l.pos:i]
		l.advance(i - l.pos)
		return start, nil
	case c == '-' || c >= '0' && c <= '9':
		i := l.pos + 1
		float := false
		for i < len(l.src) && (l.src[i] >= '0' && l.src[i] <= '9' || strings.IndexByte(".eE+-", l.src[i]) >= 0) {
			if strings.IndexByte(".eE", l.src[i]) >= 0 {
				float = true
			}
			i++
		}
		start.kind, start.val = tokInt, l.src[l.pos:i]
		if float {
			start.kind = tokFloat
		}
		l.advance(i - l.pos)
		return start, nil
	case c == '"':
		s, err := l.str()
		if err != nil {
			return gqlTok{}, err
		}
		start.kind, start.val = tokString, s
		return start, nil
	}
	return gqlTok{}, &gqlSyntaxError{msg: fmt.Sprintf("Parse error on %q (error) at [%d, %d]", string(c), l.line, l.col), line: l.line, col: l.col}
}

// advance moves n bytes, tracking lines and columns.
func (l *lexer) advance(n int) {
	for i := 0; i < n && l.pos < len(l.src); i++ {
		if l.src[l.pos] == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos++
	}
}

// skip skips whitespace, commas, comments and a BOM.
func (l *lexer) skip() {
	for l.pos < len(l.src) {
		switch c := l.src[l.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			l.advance(1)
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.advance(1)
			}
		case strings.HasPrefix(l.src[l.pos:], "\xef\xbb\xbf"):
			l.pos += len("\xef\xbb\xbf")
		default:
			return
		}
	}
}

// str reads a string or block string.
func (l *lexer) str() (string, error) {
	line, col := l.line, l.col
	bad := func() (string, error) {
		return "", &gqlSyntaxError{msg: fmt.Sprintf("Parse error on bad Unicode escape sequence or unterminated string at [%d, %d]", line, col), line: line, col: col}
	}
	if strings.HasPrefix(l.src[l.pos:], `"""`) {
		l.advance(3)
		end := strings.Index(l.src[l.pos:], `"""`)
		if end < 0 {
			return bad()
		}
		raw := l.src[l.pos : l.pos+end]
		l.advance(end + 3)
		return blockString(strings.ReplaceAll(raw, `\"""`, `"""`)), nil
	}
	l.advance(1)
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '"':
			l.advance(1)
			return b.String(), nil
		case '\n':
			return bad()
		case '\\':
			if l.pos+1 >= len(l.src) {
				return bad()
			}
			e := l.src[l.pos+1]
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if l.pos+6 > len(l.src) {
					return bad()
				}
				n, err := strconv.ParseUint(l.src[l.pos+2:l.pos+6], 16, 32)
				if err != nil {
					return bad()
				}
				b.WriteRune(rune(n))
				l.advance(4)
			default:
				return bad()
			}
			l.advance(2)
		default:
			r, size := utf8.DecodeRuneInString(l.src[l.pos:])
			b.WriteRune(r)
			l.pos += size
			l.col++
		}
	}
	return bad()
}

// blockString applies the block string indentation rules.
func blockString(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	indent := -1
	for _, line := range lines[1:] {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" {
			continue
		}
		if n := len(line) - len(trimmed); indent < 0 || n < indent {
			indent = n
		}
	}
	if indent > 0 {
		for i := 1; i < len(lines); i++ {
			if len(lines[i]) >= indent {
				lines[i] = lines[i][indent:]
			} else {
				lines[i] = strings.TrimLeft(lines[i], " \t")
			}
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Value kinds.
const (
	valVar = iota + 1
	valInt
	valFloat
	valString
	valBool
	valNull
	valEnum
	valList
	valObject
)

// gqlValue is an input value literal.
type gqlValue struct {
	kind int
	s    string
	list []gqlValue
	obj  []gqlArg
	line int
	col  int
}

// gqlArg is a named value (an argument or an object field).
type gqlArg struct {
	name string
	val  gqlValue
}

// gqlTypeRef is a type reference: Name, [T], T!.
type gqlTypeRef struct {
	name    string
	elem    *gqlTypeRef
	nonNull bool
}

// String formats the reference as GraphQL does.
func (t *gqlTypeRef) String() string {
	s := t.name
	if t.elem != nil {
		s = "[" + t.elem.String() + "]"
	}
	if t.nonNull {
		s += "!"
	}
	return s
}

// named returns the innermost type name.
func (t *gqlTypeRef) named() string {
	for t.elem != nil {
		t = t.elem
	}
	return t.name
}

// Selection kinds.
const (
	selField = iota + 1
	selInline
	selSpread
)

// gqlSel is a field, an inline fragment or a fragment spread.
type gqlSel struct {
	kind       int
	alias      string
	name       string
	args       []gqlArg
	sel        []gqlSel
	typeCond   string
	directives []gqlDirective
	line, col  int
}

// key is the response key of a field.
func (s *gqlSel) key() string {
	if s.alias != "" {
		return s.alias
	}
	return s.name
}

// gqlDirective is @name(args).
type gqlDirective struct {
	name string
	args []gqlArg
}

// gqlVarDef is a variable definition.
type gqlVarDef struct {
	name      string
	typ       *gqlTypeRef
	def       *gqlValue
	line, col int
}

// gqlOp is an operation.
type gqlOp struct {
	typ       string // "query", "mutation" or "subscription"
	name      string
	vars      []gqlVarDef
	sel       []gqlSel
	line, col int
}

// label names the operation in validation error paths: "query",
// "query Name", "mutation Name" (observed read-only 2026-09-29 for an
// anonymous query).
func (o *gqlOp) label() string {
	if o.name == "" {
		return o.typ
	}
	return o.typ + " " + o.name
}

// gqlFrag is a named fragment.
type gqlFrag struct {
	name, typeCond string
	sel            []gqlSel
}

// gqlDoc is an executable document.
type gqlDoc struct {
	ops   []*gqlOp
	frags map[string]*gqlFrag
}

// parser parses tokens of a lexer.
type parser struct {
	lx  *lexer
	tok gqlTok
}

// newParser starts a parser on src.
func newParser(src string) (*parser, error) {
	p := &parser{lx: &lexer{src: src, line: 1, col: 1}}
	return p, p.advance()
}

func (p *parser) advance() error {
	t, err := p.lx.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

// fail reports an unexpected token the way GitHub does.
func (p *parser) fail() error {
	what := p.tok.val
	kind := "unexpected token"
	switch p.tok.kind {
	case tokEOF:
		what, kind = "end of file", "EOF"
	case tokPunct:
		kind = punctName(p.tok.val)
	case tokName:
		kind = "IDENTIFIER"
	case tokString:
		kind = "STRING"
	case tokInt:
		kind = "INT"
	case tokFloat:
		kind = "FLOAT"
	}
	msg := fmt.Sprintf("Parse error on %q (%s) at [%d, %d]", what, kind, p.tok.line, p.tok.col)
	if p.tok.kind == tokEOF {
		msg = "Unexpected end of document"
	}
	return &gqlSyntaxError{msg: msg, line: p.tok.line, col: p.tok.col}
}

// punctName names punctuators as GitHub's parse errors do.
func punctName(v string) string {
	switch v {
	case "{":
		return "LCURLY"
	case "}":
		return "RCURLY"
	case "(":
		return "LPAREN"
	case ")":
		return "RPAREN"
	case "[":
		return "LBRACKET"
	case "]":
		return "RBRACKET"
	case ":":
		return "COLON"
	case "$":
		return "VAR_SIGN"
	case "!":
		return "BANG"
	case "=":
		return "EQUALS"
	case "@":
		return "DIR_SIGN"
	case "...":
		return "ELLIPSIS"
	}
	return "PUNCT"
}

// is reports whether the current token is punctuator v.
func (p *parser) is(v string) bool { return p.tok.kind == tokPunct && p.tok.val == v }

// expect consumes punctuator v.
func (p *parser) expect(v string) error {
	if !p.is(v) {
		return p.fail()
	}
	return p.advance()
}

// name consumes a name.
func (p *parser) name() (string, error) {
	if p.tok.kind != tokName {
		return "", p.fail()
	}
	v := p.tok.val
	return v, p.advance()
}

// parseDocument parses an executable document.
func parseDocument(src string) (*gqlDoc, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	doc := &gqlDoc{frags: map[string]*gqlFrag{}}
	for p.tok.kind != tokEOF {
		switch {
		case p.is("{"):
			op := &gqlOp{typ: "query", line: p.tok.line, col: p.tok.col}
			if op.sel, err = p.selectionSet(); err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, op)
		case p.tok.kind == tokName && (p.tok.val == "query" || p.tok.val == "mutation" || p.tok.val == "subscription"):
			op, err := p.operation()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, op)
		case p.tok.kind == tokName && p.tok.val == "fragment":
			if err := p.advance(); err != nil {
				return nil, err
			}
			f := &gqlFrag{}
			if f.name, err = p.name(); err != nil {
				return nil, err
			}
			if p.tok.kind != tokName || p.tok.val != "on" {
				return nil, p.fail()
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
			if f.typeCond, err = p.name(); err != nil {
				return nil, err
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			if f.sel, err = p.selectionSet(); err != nil {
				return nil, err
			}
			doc.frags[f.name] = f
		default:
			return nil, p.fail()
		}
	}
	if len(doc.ops) == 0 {
		return nil, &gqlSyntaxError{msg: "Unexpected end of document", line: 1, col: 1}
	}
	return doc, nil
}

// operation parses "query Name($v: T = d) @dir { … }".
func (p *parser) operation() (*gqlOp, error) {
	op := &gqlOp{typ: p.tok.val, line: p.tok.line, col: p.tok.col}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind == tokName {
		op.name = p.tok.val
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if p.is("(") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		for !p.is(")") {
			v := gqlVarDef{line: p.tok.line, col: p.tok.col}
			if err := p.expect("$"); err != nil {
				return nil, err
			}
			var err error
			if v.name, err = p.name(); err != nil {
				return nil, err
			}
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			if v.typ, err = p.typeRef(); err != nil {
				return nil, err
			}
			if p.is("=") {
				if err := p.advance(); err != nil {
					return nil, err
				}
				d, err := p.value(true)
				if err != nil {
					return nil, err
				}
				v.def = &d
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			op.vars = append(op.vars, v)
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if _, err := p.directives(); err != nil {
		return nil, err
	}
	var err error
	op.sel, err = p.selectionSet()
	return op, err
}

// typeRef parses a type reference.
func (p *parser) typeRef() (*gqlTypeRef, error) {
	var t *gqlTypeRef
	if p.is("[") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		elem, err := p.typeRef()
		if err != nil {
			return nil, err
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		t = &gqlTypeRef{elem: elem}
	} else {
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		t = &gqlTypeRef{name: n}
	}
	if p.is("!") {
		t.nonNull = true
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// selectionSet parses "{ selection… }".
func (p *parser) selectionSet() ([]gqlSel, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []gqlSel
	for !p.is("}") {
		if p.tok.kind == tokEOF {
			return nil, p.fail()
		}
		s, err := p.selection()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, p.fail()
	}
	return out, p.advance()
}

// selection parses a field, "... on T { }", "... @dir { }" or "...Name".
func (p *parser) selection() (gqlSel, error) {
	s := gqlSel{line: p.tok.line, col: p.tok.col}
	var err error
	if p.is("...") {
		if err := p.advance(); err != nil {
			return s, err
		}
		if p.tok.kind == tokName && p.tok.val != "on" {
			s.kind = selSpread
			s.name = p.tok.val
			if err := p.advance(); err != nil {
				return s, err
			}
			s.directives, err = p.directives()
			return s, err
		}
		s.kind = selInline
		if p.tok.kind == tokName && p.tok.val == "on" {
			if err := p.advance(); err != nil {
				return s, err
			}
			if s.typeCond, err = p.name(); err != nil {
				return s, err
			}
		}
		if s.directives, err = p.directives(); err != nil {
			return s, err
		}
		s.sel, err = p.selectionSet()
		return s, err
	}
	s.kind = selField
	if s.name, err = p.name(); err != nil {
		return s, err
	}
	if p.is(":") {
		if err := p.advance(); err != nil {
			return s, err
		}
		s.alias = s.name
		if s.name, err = p.name(); err != nil {
			return s, err
		}
	}
	if s.args, err = p.arguments(false); err != nil {
		return s, err
	}
	if s.directives, err = p.directives(); err != nil {
		return s, err
	}
	if p.is("{") {
		s.sel, err = p.selectionSet()
	}
	return s, err
}

// arguments parses "(name: value, …)" when present.
func (p *parser) arguments(constant bool) ([]gqlArg, error) {
	if !p.is("(") {
		return nil, nil
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	var out []gqlArg
	for !p.is(")") {
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		v, err := p.value(constant)
		if err != nil {
			return nil, err
		}
		out = append(out, gqlArg{name: n, val: v})
	}
	return out, p.advance()
}

// directives parses "@name(args)…".
func (p *parser) directives() ([]gqlDirective, error) {
	var out []gqlDirective
	for p.is("@") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		args, err := p.arguments(false)
		if err != nil {
			return nil, err
		}
		out = append(out, gqlDirective{name: n, args: args})
	}
	return out, nil
}

// value parses an input value.
func (p *parser) value(constant bool) (gqlValue, error) {
	v := gqlValue{line: p.tok.line, col: p.tok.col}
	switch p.tok.kind {
	case tokPunct:
		switch p.tok.val {
		case "$":
			if constant {
				return v, p.fail()
			}
			if err := p.advance(); err != nil {
				return v, err
			}
			n, err := p.name()
			v.kind, v.s = valVar, n
			return v, err
		case "[":
			if err := p.advance(); err != nil {
				return v, err
			}
			v.kind = valList
			for !p.is("]") {
				item, err := p.value(constant)
				if err != nil {
					return v, err
				}
				v.list = append(v.list, item)
			}
			return v, p.advance()
		case "{":
			if err := p.advance(); err != nil {
				return v, err
			}
			v.kind = valObject
			for !p.is("}") {
				n, err := p.name()
				if err != nil {
					return v, err
				}
				if err := p.expect(":"); err != nil {
					return v, err
				}
				item, err := p.value(constant)
				if err != nil {
					return v, err
				}
				v.obj = append(v.obj, gqlArg{name: n, val: item})
			}
			return v, p.advance()
		}
		return v, p.fail()
	case tokInt:
		v.kind, v.s = valInt, p.tok.val
	case tokFloat:
		v.kind, v.s = valFloat, p.tok.val
	case tokString:
		v.kind, v.s = valString, p.tok.val
	case tokName:
		switch p.tok.val {
		case "true", "false":
			v.kind, v.s = valBool, p.tok.val
		case "null":
			v.kind = valNull
		default:
			v.kind, v.s = valEnum, p.tok.val
		}
	default:
		return v, p.fail()
	}
	return v, p.advance()
}

// String formats a literal as GraphQL source, for error messages.
func (v gqlValue) String() string {
	switch v.kind {
	case valVar:
		return "$" + v.s
	case valString:
		return strconv.Quote(v.s)
	case valNull:
		return "null"
	case valList:
		parts := make([]string, len(v.list))
		for i, item := range v.list {
			parts[i] = item.String()
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case valObject:
		parts := make([]string, len(v.obj))
		for i, f := range v.obj {
			parts[i] = f.name + ": " + f.val.String()
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return v.s
}
