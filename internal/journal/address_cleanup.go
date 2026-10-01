package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"

	"github.com/dashpay/dash-network-go/internal/provision"
)

// AddressCleanup returns a one-run store for retiring detached addresses only.
// It never creates an operation or relaxes Dynamo's normal deployment codec.
// Application history is validated as JSON/schema on read, but not against the
// current release's component rules, and is preserved verbatim on every write.
func (d Dynamo) AddressCleanup() provision.Store {
	return &addressCleanupStore{d: d}
}

type addressCleanupStore struct {
	d         Dynamo
	plan      provision.Plan
	owner     string
	history   map[string]json.RawMessage
	canonical []byte
}

var historyFields = []string{"bootstrap", "deployment", "join", "runtime", "upgrade"}

func applicationHistory(r provision.Record) ([]byte, error) {
	return json.Marshal([]any{r.Bootstrap, r.Deployment, r.Join, r.Runtime, r.Upgrade})
}

func (s *addressCleanupStore) Acquire(ctx context.Context, p provision.Plan, owner string) (provision.Record, error) {
	if owner == "" || s.owner != "" {
		return provision.Record{}, errors.New("cleanup requires a fresh store and runner identity")
	}
	// Cleanup needs the existing immutable journal; it must not create evidence.
	item, err := s.d.readItem(ctx, p)
	if err != nil {
		return provision.Record{}, err
	}
	if _, err = decodeRecord(item, p, provision.Record.ValidateAddressCleanup); err != nil {
		return provision.Record{}, err
	}
	item, err = s.d.claim(ctx, p, owner)
	if err != nil {
		return provision.Record{}, err
	}
	// Validate the actual AllNew claim response, not the pre-claim snapshot.
	// An ambiguous or invalid response retains the claim for explicit recovery.
	r, err := decodeRecord(item, p, provision.Record.ValidateAddressCleanup)
	if err != nil {
		return r, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal([]byte(value(item, "Data")), &fields); err != nil {
		return r, err
	}
	s.history = map[string]json.RawMessage{}
	for _, field := range historyFields {
		if raw, ok := fields[field]; ok {
			s.history[field] = raw
		}
	}
	s.canonical, err = applicationHistory(r)
	if err != nil {
		return r, err
	}
	s.plan, s.owner = r.Plan, owner
	return r, nil
}

func (s *addressCleanupStore) Save(ctx context.Context, r provision.Record, owner string) error {
	if s.owner == "" || owner != s.owner || !reflect.DeepEqual(r.Plan, s.plan) || r.Revision < 1 {
		return errors.New("cleanup save lost its acquired plan, runner, or revision")
	}
	if err := r.ValidateAddressCleanup(s.plan); err != nil {
		return err
	}
	history, err := applicationHistory(r)
	if err != nil {
		return err
	}
	if !bytes.Equal(history, s.canonical) {
		return errors.New("address cleanup cannot change application history")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, field := range historyFields {
		delete(fields, field)
	}
	for field, raw := range s.history {
		fields[field] = raw
	}
	// Marshal of RawMessage compacts/escapes JSON. Assemble the outer object so
	// even historical whitespace, explicit null/zero fields and escaping survive.
	keys := make([]string, 0, len(fields))
	for field := range fields {
		keys = append(keys, field)
	}
	sort.Strings(keys)
	var payload bytes.Buffer
	payload.WriteByte('{')
	for i, field := range keys {
		if i > 0 {
			payload.WriteByte(',')
		}
		name, _ := json.Marshal(field)
		payload.Write(name)
		payload.WriteByte(':')
		payload.Write(fields[field])
	}
	payload.WriteByte('}')
	if payload.Len() > 300000 {
		return errors.New("operation journal exceeds conservative DynamoDB item budget")
	}
	return s.d.save(ctx, r, owner, payload.String())
}

func (s *addressCleanupStore) Release(ctx context.Context, p provision.Plan, owner string) error {
	if s.owner == "" || owner != s.owner || !reflect.DeepEqual(p, s.plan) {
		return errors.New("cleanup release lost its acquired plan or runner")
	}
	if err := s.d.Release(ctx, p, owner); err != nil {
		return err
	}
	s.owner = ""
	return nil
}
