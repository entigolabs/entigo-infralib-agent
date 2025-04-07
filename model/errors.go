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

type FileNotFoundError struct {
	fileName string
}

func NewFileNotFoundError(fileName string) FileNotFoundError {
	return FileNotFoundError{fileName: fileName}
}

func (e FileNotFoundError) Error() string {
	return fmt.Sprintf("file %s not found", e.fileName)
}

func (e FileNotFoundError) Unwrap() error {
	return nil
}
