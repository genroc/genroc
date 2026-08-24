package model

import "fmt"

// Context reads an instance's decoded context by PATH, loading externalized values only where a
// walk has to step through one. specs/lazy-context.md.
//
// The decoded context already carries an *ObjectRef at the path it was cut from, so the three
// cases fall out of an ordinary walk with no path comparison:
//
//   - the walk meets a marker and must continue past it  -> load it, walk on
//   - the walk ends above a marker                       -> return the subtree, marker intact
//   - the walk never meets one                           -> load nothing
//
// The second is what lets an untouched value reach the next write as the reference it already
// was: whoever copies that subtree copies the marker, and the write re-emits it.
//
// Context NEVER writes a loaded value back into the data. Materializing into the context would
// destroy exactly the markers the write path needs, and the next write would re-marshal and
// re-hash a value to arrive at the hash it came off disk with.
type Context struct {
	data map[string]any
	load func(hash string) (any, error)
	memo map[string]any
}

func NewContext(data map[string]any, load func(hash string) (any, error), memo map[string]any) *Context {
	if memo == nil {
		memo = map[string]any{}
	}
	return &Context{data: data, load: load, memo: memo}
}

// Data is the underlying context map, markers intact. For callers that write slots rather than
// read them; a reader wants At.
func (c *Context) Data() map[string]any { return c.data }

// At returns the value at path. A missing step yields nil, not an error: the path came from an
// expression, and a definition may legitimately read a slot that has not been produced yet.
func (c *Context) At(path ...any) (any, error) {
	cur := any(c.data)
	for _, seg := range path {
		// Only here -- the walk has another step to take, so the marker is in the way.
		if ref, ok := cur.(*ObjectRef); ok {
			v, err := c.object(ref)
			if err != nil {
				return nil, err
			}
			cur = v
		}
		next, ok := step(cur, seg)
		if !ok {
			return nil, nil
		}
		cur = next
	}
	return cur, nil
}

// Materialize returns v with every marker under it replaced by its value, for a consumer that
// cannot follow a reference. It COPIES: the argument may be part of the live context, whose
// markers the write path still needs.
func (c *Context) Materialize(v any) (any, error) {
	switch t := v.(type) {
	case *ObjectRef:
		loaded, err := c.object(t)
		if err != nil {
			return nil, err
		}
		return c.Materialize(loaded)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := c.Materialize(val)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := c.Materialize(val)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	}
	return v, nil
}

// MaterializeAt is At followed by Materialize -- the whole value at path, references and all.
func (c *Context) MaterializeAt(path ...any) (any, error) {
	v, err := c.At(path...)
	if err != nil {
		return nil, err
	}
	return c.Materialize(v)
}

func (c *Context) object(ref *ObjectRef) (any, error) {
	if v, ok := c.memo[ref.Ref]; ok {
		return v, nil
	}
	if c.load == nil {
		return nil, fmt.Errorf("object %s: no loader", ref.Ref)
	}
	v, err := c.load(ref.Ref)
	if err != nil {
		return nil, err
	}
	c.memo[ref.Ref] = v
	return v, nil
}

func step(v any, seg any) (any, bool) {
	switch node := v.(type) {
	case map[string]any:
		key, ok := seg.(string)
		if !ok {
			return nil, false
		}
		val, ok := node[key]
		return val, ok
	case []any:
		i, ok := index(seg)
		if !ok || i < 0 || i >= len(node) {
			return nil, false
		}
		return node[i], true
	}
	return nil, false
}
