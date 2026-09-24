package join

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dashpay/dash-network-go/internal/provision"
)

type memoryStore struct {
	record   provision.Record
	owner    string
	failSave func(provision.Record) error
}

func clone(r provision.Record) provision.Record {
	b, _ := json.Marshal(r)
	var c provision.Record
	_ = json.Unmarshal(b, &c)
	return c
}
func (s *memoryStore) Read(_ context.Context, p provision.Plan) (provision.Record, string, error) {
	if s.record.Kind == "" {
		return provision.Record{}, "", errors.New("missing")
	}
	return clone(s.record), s.owner, s.record.Validate(p)
}
func (s *memoryStore) Acquire(_ context.Context, p provision.Plan, o string) (provision.Record, error) {
	if s.owner != "" {
		return provision.Record{}, errors.New("busy")
	}
	if s.record.Kind == "" {
		s.record = provision.NewRecord(p)
	}
	if s.record.Plan.ID != p.ID {
		return provision.Record{}, errors.New("plan changed")
	}
	s.owner = o
	return clone(s.record), nil
}
func (s *memoryStore) Save(_ context.Context, r provision.Record, o string) error {
	if o != s.owner || r.Revision != s.record.Revision+1 {
		return errors.New("owner/revision lost")
	}
	if err := r.Validate(r.Plan); err != nil {
		return err
	}
	if s.failSave != nil {
		if err := s.failSave(r); err != nil {
			return err
		}
	}
	s.record = clone(r)
	return nil
}
func (s *memoryStore) Release(_ context.Context, _ provision.Plan, o string) error {
	if o != s.owner {
		return errors.New("wrong owner")
	}
	s.owner = ""
	return nil
}
