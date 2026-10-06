package ghfake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The GraphQL endpoint. The contract with drivers: the fake parses every
// request as GraphQL, validates the chosen operation against schemaSDL
// (unknown fields, arguments, types, fragments and variables fail with
// GitHub's messages and codes, and HTTP 200 without data), then
// executes it over its state: any query or mutation that stays within the
// subset works, whatever its operation name, aliases and fragments. The
// operation name only labels the request log and error paths. Fault
// injection addresses a request by "POST /graphql" or by one of the root
// fields it selects ("GRAPHQL updateRefs").

// gqlError is one error of a response. Its fields are in GitHub's order
// (observed read-only 2026-09-29).
type gqlError struct {
	Type       string           `json:"type,omitempty"`
	Path       []any            `json:"path,omitempty"`
	Extensions map[string]any   `json:"extensions,omitempty"`
	Locations  []map[string]int `json:"locations,omitempty"`
	Message    string           `json:"message"`
}

// at returns the locations of a position.
func at(line, col int) []map[string]int {
	return []map[string]int{{"line": line, "column": col}}
}

// orderedMap is a JSON object that keeps its keys in selection order.
type orderedMap struct {
	keys []string
	vals map[string]any
}

func newOrderedMap() *orderedMap { return &orderedMap{vals: map[string]any{}} }

// set adds or replaces a key.
func (m *orderedMap) set(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

// MarshalJSON writes the object in key order.
func (m *orderedMap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(m.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// gqlRequest is the body of a GraphQL request.
type gqlRequest struct {
	Query         string          `json:"query"`
	Variables     json.RawMessage `json:"variables"`
	OperationName string          `json:"operationName"`
}

// serveGraphQL answers POST /graphql and /api/graphql.
func (s *Server) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	route := "POST /graphql"
	who := "anonymous"
	finish := func(resp response) {
		s.mu.Lock()
		s.logRequest(r.Method, route, who, resp.status)
		s.mu.Unlock()
		writeResponse(w, resp)
	}
	if r.Method != http.MethodPost {
		finish(notFound())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	var req gqlRequest
	if err != nil || len(body) > maxRequestBody || json.Unmarshal(body, &req) != nil {
		finish(apiError(http.StatusBadRequest, "Problems parsing JSON"))
		return
	}
	doc, perr := parseDocument(req.Query)
	var op *gqlOp
	if perr == nil {
		op, perr = pickOperation(doc, req.OperationName)
	}
	mutation := op != nil && op.typ == "mutation"
	if op != nil {
		route += " " + strings.Join(rootFields(op, doc), ",")
	}
	id, resp, ok := s.authenticate(withMethod(r, mutation))
	if id != nil {
		who = id.key()
	}
	switch {
	case !ok:
		finish(resp)
		return
	case id == nil:
		finish(apiError(http.StatusUnauthorized, "This endpoint requires you to be authenticated."))
		return
	case id.kind == idJWT:
		finish(apiError(http.StatusForbidden, "Resource not accessible by integration"))
		return
	}
	points := 1
	if mutation {
		points = 5
	}
	s.mu.Lock()
	limited, lh := s.limits.admit(s.now(), id, points, false, "graphql")
	var fault *Fault
	faulted := false
	if limited == nil {
		keys := []string{"POST /graphql"}
		if op != nil {
			for _, f := range rootFields(op, doc) {
				keys = append(keys, "GRAPHQL "+f)
			}
		}
		fault, faulted = s.takeFault(keys...)
	}
	s.mu.Unlock()
	if limited != nil {
		if limited.status == http.StatusForbidden && strings.HasPrefix(limited.body.(map[string]any)["message"].(string), "API rate limit") {
			// The primary GraphQL limit answers 200 with an error (documented).
			msg := limited.body.(map[string]any)["message"].(string)
			finish(withHeader(ok200(map[string]any{"errors": []gqlError{{Type: "RATE_LIMITED", Message: msg}}}), lh))
			return
		}
		finish(withHeader(*limited, lh))
		return
	}
	if faulted && !fault.Applied {
		finish(withHeader(gqlFault(fault), lh))
		return
	}
	var out any
	if perr != nil {
		out = map[string]any{"errors": []gqlError{syntaxError(perr)}}
	} else {
		out = s.execute(r.Context(), id, doc, op, req.Variables)
	}
	resp = withHeader(ok200(out), lh)
	if faulted {
		resp = withHeader(gqlFault(fault), lh)
	}
	finish(resp)
}

// ok200 answers 200 with a GraphQL body.
func ok200(body any) response { return response{status: http.StatusOK, body: body} }

// withMethod makes authenticate see a mutation as a write.
func withMethod(r *http.Request, mutation bool) *http.Request {
	if !mutation {
		out := r.Clone(r.Context())
		out.Method = http.MethodGet
		return out
	}
	return r
}

// gqlFault is the response of a fault on the GraphQL endpoint.
func gqlFault(f *Fault) response {
	if f.GraphQL != "" {
		msg := f.Message
		if msg == "" {
			msg = "Something went wrong while executing your query."
		}
		return ok200(map[string]any{"errors": []gqlError{{Type: f.GraphQL, Message: msg}}})
	}
	return faultResponse(f)
}

// syntaxError converts a parse error.
func syntaxError(err error) gqlError {
	var se *gqlSyntaxError
	if errors.As(err, &se) {
		e := gqlError{Message: se.msg}
		if se.line > 0 {
			e.Locations = at(se.line, se.col)
		}
		return e
	}
	return gqlError{Message: err.Error()}
}

// pickOperation chooses the operation to run.
func pickOperation(doc *gqlDoc, name string) (*gqlOp, error) {
	if name == "" {
		if len(doc.ops) > 1 {
			return nil, &gqlSyntaxError{msg: "An operation name is required"}
		}
		return doc.ops[0], nil
	}
	for _, op := range doc.ops {
		if op.name == name {
			return op, nil
		}
	}
	return nil, &gqlSyntaxError{msg: "No operation named \"" + name + "\""}
}

// rootFields lists the root fields an operation selects, sorted.
func rootFields(op *gqlOp, doc *gqlDoc) []string {
	seen := map[string]bool{}
	var walk func([]gqlSel, int)
	walk = func(sels []gqlSel, depth int) {
		if depth > 10 {
			return
		}
		for _, s := range sels {
			switch s.kind {
			case selField:
				seen[s.name] = true
			case selInline:
				walk(s.sel, depth+1)
			case selSpread:
				if f := doc.frags[s.name]; f != nil {
					walk(f.sel, depth+1)
				}
			}
		}
	}
	walk(op.sel, 0)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// executor runs one operation.
type executor struct {
	s      *Server
	ctx    context.Context
	id     *identity
	sc     *gqlSchema
	doc    *gqlDoc
	op     *gqlOp
	vars   map[string]any
	errors []gqlError
	// blobs are the blobs this operation read, by repository directory and
	// object id: a blob's byteSize, isBinary, isTruncated and text then
	// cost one git process, not four. Objects are content-addressed, so a
	// read never goes stale.
	blobs map[string][]byte
}

// blob returns the content of blob oid of r, read once per operation.
func (e *executor) blob(r *repo, oid string) ([]byte, error) {
	key := r.dir + "\x00" + oid
	if data, ok := e.blobs[key]; ok {
		return data, nil
	}
	data, err := e.s.repoGit(r).catFile(e.ctx, "blob", oid)
	if err != nil {
		return nil, err
	}
	if e.blobs == nil {
		e.blobs = map[string][]byte{}
	}
	e.blobs[key] = data
	return data, nil
}

// execute validates and runs an operation and returns the response body.
// A mutation runs under gitMu; everything under mu.
func (s *Server) execute(ctx context.Context, id *identity, doc *gqlDoc, op *gqlOp, rawVars json.RawMessage) any {
	sc, err := fakeSchema(s.flavor)
	if err != nil {
		return map[string]any{"errors": []gqlError{{Message: "ghfake: " + err.Error()}}}
	}
	e := &executor{s: s, ctx: ctx, id: id, sc: sc, doc: doc, op: op}
	root := sc.types["Query"]
	if op.typ == "mutation" {
		root = sc.types["Mutation"]
	}
	if op.typ == "subscription" || root == nil {
		return map[string]any{"errors": []gqlError{{Message: "Schema is not configured for " + op.typ + "s"}}}
	}
	e.validate(root, op.sel, []any{op.label()}, map[string]bool{})
	e.checkVarUse()
	if len(e.errors) > 0 {
		return map[string]any{"errors": e.errors}
	}
	if e.coerceVars(rawVars); len(e.errors) > 0 {
		return map[string]any{"errors": e.errors}
	}
	if op.typ == "mutation" {
		s.gitMu.Lock()
		defer s.gitMu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var data *orderedMap
	if op.typ == "mutation" {
		data = e.execSel(mutationRoot{}, root, op.sel, nil)
	} else {
		data = e.execSel(queryRoot{}, root, op.sel, nil)
	}
	out := newOrderedMap()
	out.set("data", data)
	if len(e.errors) > 0 {
		out.set("errors", e.errors)
	}
	return out
}

// fail records a validation error.
func (e *executor) fail(path []any, line, col int, code string, ext map[string]any, msg string) {
	if ext == nil {
		ext = map[string]any{}
	}
	ext["code"] = code
	e.errors = append(e.errors, gqlError{Path: slices.Clone(path), Extensions: ext, Locations: at(line, col), Message: msg})
}

// validate checks a selection set against its parent type.
func (e *executor) validate(parent *gqlType, sels []gqlSel, path []any, frags map[string]bool) {
	for i := range sels {
		s := &sels[i]
		e.validateDirectives(s, path)
		switch s.kind {
		case selInline:
			t := parent
			if s.typeCond != "" {
				t = e.sc.types[s.typeCond]
				if t == nil {
					e.fail(append(path, "... on "+s.typeCond), s.line, s.col, "undefinedType",
						map[string]any{"typeName": s.typeCond}, "No such type "+s.typeCond+", so it can't be a fragment condition")
					continue
				}
				if !e.sc.overlaps(parent, t) {
					e.fail(append(path, "... on "+s.typeCond), s.line, s.col, "fragmentSpreadTypeMismatch", nil,
						"Fragment on "+s.typeCond+" can't be spread inside "+parent.name)
					continue
				}
			}
			e.validate(t, s.sel, append(path, "... on "+t.name), frags)
		case selSpread:
			f := e.doc.frags[s.name]
			if f == nil {
				e.fail(append(path, "... "+s.name), s.line, s.col, "useAndDefineFragment", map[string]any{"fragmentName": s.name},
					"Fragment "+s.name+" was used, but not defined")
				continue
			}
			if frags[s.name] {
				e.fail(append(path, "... "+s.name), s.line, s.col, "infiniteLoop", nil,
					"Fragment "+s.name+" contains an infinite loop")
				continue
			}
			t := e.sc.types[f.typeCond]
			if t == nil {
				e.fail(append(path, "... "+s.name), s.line, s.col, "undefinedType", map[string]any{"typeName": f.typeCond},
					"No such type "+f.typeCond+", so it can't be a fragment condition")
				continue
			}
			inner := map[string]bool{s.name: true}
			for k := range frags {
				inner[k] = true
			}
			e.validate(t, f.sel, append(path, "... "+s.name), inner)
		default:
			e.validateField(parent, s, path, frags)
		}
	}
}

// validateDirectives accepts @include(if:) and @skip(if:) only.
func (e *executor) validateDirectives(s *gqlSel, path []any) {
	for _, d := range s.directives {
		if d.name != "include" && d.name != "skip" {
			e.fail(path, s.line, s.col, "undefinedDirective", map[string]any{"directiveName": d.name},
				"Directive @"+d.name+" is not defined")
			continue
		}
		if len(d.args) != 1 || d.args[0].name != "if" {
			e.fail(path, s.line, s.col, "missingRequiredArguments", nil, "Directive '"+d.name+"' is missing required arguments: if")
		}
	}
}

// validateField checks one field: it exists, its arguments are known,
// present when required and of the right type, and it has a selection
// set exactly when its type is composite.
func (e *executor) validateField(parent *gqlType, s *gqlSel, path []any, frags map[string]bool) {
	p := append(path, s.key())
	if s.name == "__typename" {
		if len(s.sel) > 0 {
			e.fail(p, s.line, s.col, "selectionMismatch", nil, "Selections can't be made on scalars (field '__typename' returns String but has selections [...])")
		}
		return
	}
	f := parent.fields[s.name]
	if f == nil {
		e.fail(p, s.line, s.col, "undefinedField", map[string]any{"typeName": parent.name, "fieldName": s.name},
			"Field '"+s.name+"' doesn't exist on type '"+parent.name+"'")
		return
	}
	for _, a := range s.args {
		def := f.args[a.name]
		if def == nil {
			e.fail(p, s.line, s.col, "argumentNotAccepted",
				map[string]any{"name": s.name, "typeName": "Field", "argumentName": a.name},
				"Field '"+s.name+"' doesn't accept argument '"+a.name+"'")
			continue
		}
		if !e.literalFits(a.val, def.typ) {
			e.fail(p, s.line, s.col, "argumentLiteralsIncompatible",
				map[string]any{"typeName": "Field", "argumentName": a.name},
				fmt.Sprintf("Argument '%s' on Field '%s' has an invalid value (%s). Expected type '%s'.", a.name, s.name, a.val, def.typ))
		}
	}
	var missing []string
	for name, def := range f.args {
		if def.typ.nonNull && def.def == nil && !slices.ContainsFunc(s.args, func(a gqlArg) bool { return a.name == name }) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		e.fail(p, s.line, s.col, "missingRequiredArguments",
			map[string]any{"className": "Field", "name": s.name, "arguments": strings.Join(missing, ", ")},
			"Field '"+s.name+"' is missing required arguments: "+strings.Join(missing, ", "))
	}
	t := e.sc.types[f.typ.named()]
	composite := t.kind == kindObject || t.kind == kindInterface || t.kind == kindUnion
	switch {
	case composite && len(s.sel) == 0:
		e.fail(p, s.line, s.col, "selectionMismatch", map[string]any{"nodeName": "field '" + s.name + "'", "typeName": t.name},
			"Field '"+s.name+"' returns "+t.name+" but has no selections. Did you mean '"+s.name+" { ... }'?")
	case !composite && len(s.sel) > 0:
		e.fail(p, s.line, s.col, "selectionMismatch", map[string]any{"nodeName": "field '" + s.name + "'", "typeName": t.name},
			"Selections can't be made on scalars (field '"+s.name+"' returns "+t.name+" but has selections [...])")
	case composite:
		e.validate(t, s.sel, p, frags)
	}
}

// literalFits checks a literal against an input type. Variables are
// checked when they are coerced.
func (e *executor) literalFits(v gqlValue, t *gqlTypeRef) bool {
	switch {
	case v.kind == valVar:
		return true
	case v.kind == valNull:
		return !t.nonNull
	case t.elem != nil:
		if v.kind != valList {
			return e.literalFits(v, t.elem)
		}
		for _, item := range v.list {
			if !e.literalFits(item, t.elem) {
				return false
			}
		}
		return true
	}
	def := e.sc.types[t.name]
	switch def.kind {
	case kindEnum:
		return v.kind == valEnum && def.values[v.s]
	case kindInput:
		if v.kind != valObject {
			return false
		}
		seen := map[string]bool{}
		for _, f := range v.obj {
			in := def.inputs[f.name]
			if in == nil || !e.literalFits(f.val, in.typ) {
				return false
			}
			seen[f.name] = true
		}
		for name, in := range def.inputs {
			if in.typ.nonNull && in.def == nil && !seen[name] {
				return false
			}
		}
		return true
	}
	switch t.name {
	case "Int":
		return v.kind == valInt
	case "Float":
		return v.kind == valInt || v.kind == valFloat
	case "Boolean":
		return v.kind == valBool
	case "ID":
		return v.kind == valString || v.kind == valInt
	}
	return v.kind == valString
}

// checkVarUse reports variables used but not declared, and declared but
// not used (GitHub: "variableNotUsed" with the operation as the path and
// its position, observed read-only 2026-09-29).
func (e *executor) checkVarUse() {
	declared := map[string]bool{}
	for _, v := range e.op.vars {
		declared[v.name] = true
	}
	used := map[string]bool{}
	defer func() {
		for _, v := range e.op.vars {
			if !used[v.name] {
				e.fail([]any{e.op.label()}, e.op.line, e.op.col, "variableNotUsed", map[string]any{"variableName": v.name},
					"Variable $"+v.name+" is declared by "+e.opName()+" but not used")
			}
		}
	}()
	var walkVal func(gqlValue)
	walkVal = func(v gqlValue) {
		switch v.kind {
		case valVar:
			used[v.s] = true
			if !declared[v.s] {
				declared[v.s] = true
				e.fail(nil, v.line, v.col, "variableNotDefined", map[string]any{"variableName": v.s},
					"Variable $"+v.s+" is used by "+e.opName()+" but not declared")
			}
		case valList:
			for _, item := range v.list {
				walkVal(item)
			}
		case valObject:
			for _, f := range v.obj {
				walkVal(f.val)
			}
		}
	}
	var walk func([]gqlSel, int)
	walk = func(sels []gqlSel, depth int) {
		if depth > 20 {
			return
		}
		for _, s := range sels {
			for _, a := range s.args {
				walkVal(a.val)
			}
			for _, d := range s.directives {
				for _, a := range d.args {
					walkVal(a.val)
				}
			}
			walk(s.sel, depth+1)
			if s.kind == selSpread {
				if f := e.doc.frags[s.name]; f != nil {
					walk(f.sel, depth+1)
				}
			}
		}
	}
	walk(e.op.sel, 0)
}

// opName names the operation in messages.
func (e *executor) opName() string {
	if e.op.name == "" {
		return "anonymous " + e.op.typ
	}
	return e.op.name
}

// coerceVars checks the variables against their definitions.
func (e *executor) coerceVars(raw json.RawMessage) {
	given := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&given); err != nil {
			e.errors = append(e.errors, gqlError{Message: "Variables are invalid JSON."})
			return
		}
	}
	e.vars = map[string]any{}
	for _, def := range e.op.vars {
		v, present := given[def.name]
		switch {
		case !present && def.def != nil:
			e.vars[def.name] = e.literal(*def.def, def.typ)
			continue
		case !present:
			if def.typ.nonNull {
				e.varError(def, nil, "Expected value to not be null")
			}
			continue
		}
		out, why := e.coerce(v, def.typ)
		if why != "" {
			e.varError(def, v, why)
			continue
		}
		e.vars[def.name] = out
	}
}

// varError records an invalid variable the way GitHub does.
func (e *executor) varError(def gqlVarDef, v any, why string) {
	e.errors = append(e.errors, gqlError{
		Extensions: map[string]any{"value": v, "problems": []any{map[string]any{"path": []any{}, "explanation": why}}},
		Locations:  at(def.line, def.col),
		Message:    "Variable $" + def.name + " of type " + def.typ.String() + " was provided invalid value",
	})
}

// coerce converts a JSON variable value to t; why is set when it does not
// fit.
func (e *executor) coerce(v any, t *gqlTypeRef) (any, string) {
	if v == nil {
		if t.nonNull {
			return nil, "Expected value to not be null"
		}
		return nil, ""
	}
	if t.elem != nil {
		list, ok := v.([]any)
		if !ok {
			item, why := e.coerce(v, t.elem)
			return []any{item}, why
		}
		out := make([]any, len(list))
		for i, item := range list {
			c, why := e.coerce(item, t.elem)
			if why != "" {
				return nil, why
			}
			out[i] = c
		}
		return out, ""
	}
	def := e.sc.types[t.name]
	switch def.kind {
	case kindEnum:
		s, ok := v.(string)
		if !ok || !def.values[s] {
			return nil, fmt.Sprintf("Expected %q to be one of: %s", fmt.Sprint(v), strings.Join(sortedKeys(def.values), ", "))
		}
		return s, ""
	case kindInput:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, "Expected value to be an input object"
		}
		out := map[string]any{}
		for k := range m {
			if def.inputs[k] == nil {
				return nil, "Field is not defined on " + def.name
			}
		}
		for _, name := range def.order {
			in := def.inputs[name]
			fv, present := m[name]
			switch {
			case !present && in.def != nil:
				out[name] = e.literal(*in.def, in.typ)
			case !present && in.typ.nonNull:
				return nil, "Expected value to not be null"
			case present:
				c, why := e.coerce(fv, in.typ)
				if why != "" {
					return nil, why
				}
				out[name] = c
			}
		}
		return out, ""
	}
	switch t.name {
	case "Int":
		n, ok := v.(json.Number)
		if !ok {
			return nil, "Could not coerce value to Int"
		}
		i, err := n.Int64()
		if err != nil {
			return nil, "Could not coerce value to Int"
		}
		return i, ""
	case "Float":
		n, ok := v.(json.Number)
		if !ok {
			return nil, "Could not coerce value to Float"
		}
		f, _ := n.Float64()
		return f, ""
	case "Boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, "Could not coerce value to Boolean"
		}
		return b, ""
	case "ID":
		if n, ok := v.(json.Number); ok {
			return n.String(), ""
		}
	}
	s, ok := v.(string)
	if !ok {
		return nil, "Could not coerce value to " + t.name
	}
	return s, ""
}

// sortedKeys returns the keys of a set, sorted.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// literal converts a literal (after validation) to a Go value.
func (e *executor) literal(v gqlValue, t *gqlTypeRef) any {
	switch v.kind {
	case valVar:
		return e.vars[v.s]
	case valNull:
		return nil
	case valInt:
		if t != nil && t.named() == "Float" {
			f, _ := strconv.ParseFloat(v.s, 64)
			return f
		}
		if t != nil && t.named() == "ID" {
			return v.s
		}
		n, _ := strconv.ParseInt(v.s, 10, 64)
		return n
	case valFloat:
		f, _ := strconv.ParseFloat(v.s, 64)
		return f
	case valBool:
		return v.s == "true"
	case valString, valEnum:
		return v.s
	case valList:
		var elem *gqlTypeRef
		if t != nil {
			elem = t.elem
		}
		out := make([]any, len(v.list))
		for i, item := range v.list {
			out[i] = e.literal(item, elem)
		}
		return out
	case valObject:
		out := map[string]any{}
		var def *gqlType
		if t != nil {
			def = e.sc.types[t.named()]
		}
		for _, f := range v.obj {
			var ft *gqlTypeRef
			if def != nil && def.inputs[f.name] != nil {
				ft = def.inputs[f.name].typ
			}
			out[f.name] = e.literal(f.val, ft)
		}
		if def != nil {
			for name, in := range def.inputs {
				if _, ok := out[name]; !ok && in.def != nil {
					out[name] = e.literal(*in.def, in.typ)
				}
			}
		}
		return out
	}
	return nil
}

// args returns the arguments of a field with defaults applied; a list
// argument given a single value becomes a list.
func (e *executor) args(s *gqlSel, f *gqlField) map[string]any {
	out := map[string]any{}
	for name, def := range f.args {
		if def.def != nil {
			out[name] = e.literal(*def.def, def.typ)
		}
	}
	for _, a := range s.args {
		def := f.args[a.name]
		v := e.literal(a.val, def.typ)
		if def.typ.elem != nil && v != nil {
			if _, isList := v.([]any); !isList {
				v = []any{v}
			}
		}
		if a.val.kind == valVar {
			if _, set := e.vars[a.val.s]; !set {
				continue
			}
		}
		out[a.name] = v
	}
	return out
}

// included applies @skip and @include.
func (e *executor) included(s *gqlSel) bool {
	for _, d := range s.directives {
		if len(d.args) != 1 {
			continue
		}
		v, _ := e.literal(d.args[0].val, &gqlTypeRef{name: "Boolean"}).(bool)
		if d.name == "skip" && v || d.name == "include" && !v {
			return false
		}
	}
	return true
}

// collect gathers the fields a selection set selects on an object type,
// in order, merging fields with the same response key.
func (e *executor) collect(typename string, sels []gqlSel, out *[]*gqlSel, byKey map[string]*gqlSel, depth int) {
	if depth > 20 {
		return
	}
	for i := range sels {
		s := &sels[i]
		if !e.included(s) {
			continue
		}
		switch s.kind {
		case selField:
			if prev := byKey[s.key()]; prev != nil {
				merged := *prev
				merged.sel = append(slices.Clone(prev.sel), s.sel...)
				*prev = merged
				continue
			}
			c := *s
			byKey[s.key()] = &c
			*out = append(*out, &c)
		case selInline:
			if s.typeCond == "" || e.applies(s.typeCond, typename) {
				e.collect(typename, s.sel, out, byKey, depth+1)
			}
		case selSpread:
			if f := e.doc.frags[s.name]; f != nil && e.applies(f.typeCond, typename) {
				e.collect(typename, f.sel, out, byKey, depth+1)
			}
		}
	}
}

// applies reports whether a type condition covers an object type.
func (e *executor) applies(cond, typename string) bool {
	t := e.sc.types[cond]
	return t != nil && e.sc.possible(t, typename)
}

// gqlObject is a value of an object type.
type gqlObject interface {
	typename() string
}

// execSel executes a selection set on obj.
func (e *executor) execSel(obj gqlObject, t *gqlType, sels []gqlSel, path []any) *orderedMap {
	out := newOrderedMap()
	var fields []*gqlSel
	e.collect(obj.typename(), sels, &fields, map[string]*gqlSel{}, 0)
	for _, s := range fields {
		p := append(slices.Clone(path), s.key())
		if s.name == "__typename" {
			out.set(s.key(), obj.typename())
			continue
		}
		f := t.fields[s.name]
		if f == nil {
			out.set(s.key(), nil)
			continue
		}
		v, gerr := e.resolve(obj, s.name, e.args(s, f))
		if gerr != nil {
			gerr.Path = p
			gerr.Locations = at(s.line, s.col)
			e.errors = append(e.errors, *gerr)
			out.set(s.key(), nil)
			continue
		}
		out.set(s.key(), e.complete(v, f.typ, s, p))
	}
	return out
}

// complete shapes a resolved value by its type.
func (e *executor) complete(v any, t *gqlTypeRef, s *gqlSel, path []any) any {
	if v == nil {
		return nil
	}
	if t.elem != nil {
		list, ok := v.([]any)
		if !ok {
			return nil
		}
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = e.complete(item, t.elem, s, append(slices.Clone(path), i))
		}
		return out
	}
	def := e.sc.types[t.name]
	switch def.kind {
	case kindObject, kindInterface, kindUnion:
		obj, ok := v.(gqlObject)
		if !ok {
			return nil
		}
		ot := e.sc.types[obj.typename()]
		if ot == nil {
			return nil
		}
		return e.execSel(obj, ot, s.sel, path)
	}
	return v
}

// Validate checks a GraphQL document against the fake's schema of flavor
// f (a subset of GitHub's public schema, field for field: TestSchemaSubset)
// the way the fake validates a request: syntax, types, fields, arguments,
// fragments and the use of variables, for every operation of the
// document. It returns the messages of the errors, none for a valid
// document. Drivers' tests check their fixed queries with it.
func Validate(f Flavor, query string) []string {
	sc, err := fakeSchema(f)
	if err != nil {
		return []string{err.Error()}
	}
	doc, err := parseDocument(query)
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, op := range doc.ops {
		e := &executor{sc: sc, doc: doc, op: op}
		root := sc.types["Query"]
		if op.typ == "mutation" {
			root = sc.types["Mutation"]
		}
		if root == nil {
			out = append(out, "no root type for "+op.typ)
			continue
		}
		e.validate(root, op.sel, []any{op.label()}, map[string]bool{})
		e.checkVarUse()
		for _, ge := range e.errors {
			out = append(out, ge.Message)
		}
	}
	return out
}
