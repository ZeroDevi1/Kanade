//go:build !darwin && !windows

package mic

func devices() []string { return nil }

func start(string, func([]float32)) (func(), error) { return nil, ErrUnsupported }
