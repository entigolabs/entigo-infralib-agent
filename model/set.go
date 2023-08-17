package model

type Set[T comparable] map[T]bool

func NewSet[T comparable]() Set[T] {
	return make(Set[T])
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
