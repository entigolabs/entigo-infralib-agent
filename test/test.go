package test

import (
	"fmt"
	"github.com/brianvoe/gofakeit/v6"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/hashicorp/go-version"
	"math/rand"
	"os"
	"path"
	"runtime"
)

func ChangeRunDir() {
	_, filename, _, _ := runtime.Caller(0)
	dir := path.Join(path.Dir(filename), "..")
	err := os.Chdir(dir)
	if err != nil {
		panic(err)
	}
}

func AddFakeConfigTypes() {
	gofakeit.AddFuncLookup("version", gofakeit.Info{
		Category:    "custom",
		Description: "Random semantic version as string",
		Example:     "1.2.3",
		Output:      "string",
		Generate: func(r *rand.Rand, m *gofakeit.MapParams, info *gofakeit.Info) (interface{}, error) {
			return Version(), nil
		},
	})
	gofakeit.AddFuncLookup("semver", gofakeit.Info{
		Category:    "custom",
		Description: "Random semantic version",
		Example:     "1.2.3",
		Output:      "*version.Version",
		Generate: func(r *rand.Rand, m *gofakeit.MapParams, info *gofakeit.Info) (interface{}, error) {
			return SemVer()
		},
	})
	gofakeit.AddFuncLookup("stepType", gofakeit.Info{
		Category:    "custom",
		Description: "Random step type",
		Example:     "terraform",
		Output:      "model.StepType",
		Generate: func(r *rand.Rand, m *gofakeit.MapParams, info *gofakeit.Info) (interface{}, error) {
			return StepType(), nil
		},
	})
}

func Version() string {
	return fmt.Sprintf("%d.%d.%d", gofakeit.Number(1, 10), gofakeit.Number(1, 10), gofakeit.Number(1, 10))
}

func SemVer() (*version.Version, error) {
	return version.NewVersion(Version())
}

func StepType() model.StepType {
	return model.StepType(gofakeit.RandomString([]string{model.StepTypeArgoCD, string(model.StepTypeTerraform), model.StepTypeTerraformCustom}))
}
