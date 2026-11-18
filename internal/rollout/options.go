// Package rollout defines reviewed execution policy shared by both engines.
package rollout

import "errors"

const MaximumConcurrency = 16

type Options struct {
	MiningRecipe string   `json:"miningRecipe,omitempty"`
	Concurrency  int      `json:"concurrency"`
	PauseMining  bool     `json:"pauseMining,omitempty"`
	Miners       []string `json:"miners,omitempty"`
}

func (o Options) Validate() error {
	if o.Concurrency < 1 || o.Concurrency > MaximumConcurrency {
		return errors.New("rollout concurrency must be between 1 and 16")
	}
	if o.PauseMining != (len(o.Miners) > 0) {
		return errors.New("mining pause requires explicit owned miner names")
	}
	seen := map[string]bool{}
	for _, name := range o.Miners {
		if name == "" || seen[name] {
			return errors.New("duplicate or empty mining target")
		}
		seen[name] = true
	}
	return nil
}
