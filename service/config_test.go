package service

import (
	"github.com/brianvoe/gofakeit/v6"
	"github.com/stretchr/testify/assert"
	"testing"
)

func test(t *testing.T) {
	name := gofakeit.Name()
	assert.Equal(t, name, name, "Name is not equal")
}
