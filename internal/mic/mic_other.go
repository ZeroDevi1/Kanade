//go:build !darwin && !windows

package mic

func start(func([]float32)) (func(), error) { return nil, ErrUnsupported }
