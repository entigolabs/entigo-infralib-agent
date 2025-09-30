package model

import "fmt"

type ParameterNotFoundError struct {
	Name string
	Err  error
}

func (e *ParameterNotFoundError) Error() string {
	return fmt.Sprintf("parameter %s not found", e.Name)
}

func (e *ParameterNotFoundError) Unwrap() error {
	return e.Err
}

type NotFoundError struct {
	name string
}

func NewNotFoundError(name string) NotFoundError {
	return NotFoundError{name: name}
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("%s not found", e.name)
}

func (e NotFoundError) Unwrap() error {
	return nil
}
