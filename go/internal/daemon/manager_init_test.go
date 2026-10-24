package daemon

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
)

// Every map on Manager must be built by NewManager.
//
// Written after a nil map shipped: a new field was added to the struct and not
// to the constructor, so the first request that touched it panicked -- and,
// because the panic happened with a lock held, wedged every request after it.
// A field added later should fail here rather than in production, which is
// why this reflects over the struct instead of listing the maps it knows.
func TestNewManagerBuildsEveryMap(t *testing.T) {
	m := NewManager(NewRegistry(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.StopPersisting)
	v := reflect.ValueOf(m).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Map {
			continue
		}
		if f.IsNil() {
			t.Errorf("Manager.%s is nil; writing to it will panic", v.Type().Field(i).Name)
		}
	}
}
