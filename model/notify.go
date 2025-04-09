package model

type NotifierType struct {
	Name string
}

type Notifier interface {
	GetName() string
	Notify(message string) error
}

func (n NotifierType) GetName() string {
	return n.Name
}
