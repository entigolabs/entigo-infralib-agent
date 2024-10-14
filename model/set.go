package model

import (
	"fmt"
	"strings"
)

type Set[T comparable] map[T]bool

func NewSet[T comparable]() Set[T] {
	return make(Set[T])
}

func ToSet[T comparable](items []T) Set[T] {
	if items == nil || len(items) == 0 {
		return NewSet[T]()
	}
	set := NewSet[T]()
	for _, item := range items {
		set[item] = true
	}
	return set
}

func (s Set[T]) Add(item T) {
	s[item] = true
}

func (s Set[T]) Remove(item T) {
	delete(s, item)
}

func (s Set[T]) Contains(item T) bool {
	_, ok := s[item]
	return ok
}

func (s Set[T]) Size() int {
	return len(s)
}

func (s Set[T]) ToSlice() []T {
	slice := make([]T, 0, len(s))
	for item := range s {
		slice = append(slice, item)
	}
	return slice
}

func (s Set[T]) String() string {
	items := s.ToSlice()
	strItems := make([]string, len(items))
	for i, item := range items {
		strItems[i] = fmt.Sprintf("%v", item)
	}
	return fmt.Sprintf("%s", strings.Join(strItems, ", "))
}
