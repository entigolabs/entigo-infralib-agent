package model

import "fmt"

type ParameterNotFoundError struct {
	Name string
	Err  error
}

func (e *ParameterNotFoundError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("parameter %s not found", e.Name)
}

func (e *ParameterNotFoundError) Unwrap() error {
	return e.Err
}
