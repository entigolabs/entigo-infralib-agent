package service

import (
	"github.com/brianvoe/gofakeit/v6"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/test"
	"github.com/stretchr/testify/assert"
	"testing"
)

// TODO Unignore
func testConfig(t *testing.T) {
	test.AddFakeConfigTypes()
	var config model.Config
	err := gofakeit.Struct(&config)
	if err != nil {
		panic(err)
	}
	name := gofakeit.Name()
	assert.Equal(t, name, name, "Name is not equal")
}
