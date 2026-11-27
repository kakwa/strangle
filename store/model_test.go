package store

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gorm.io/gorm"
)

func TestModel(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Model Suite")
}

var _ = Describe("AnsibleRun UUIDv7", func() {
	It("generates monotonically increasing version-7 IDs", func() {
		var previous uuid.UUID
		for i := 0; i < 100; i++ {
			run := AnsibleRun{}
			Expect(run.BeforeCreate(&gorm.DB{})).To(Succeed())
			Expect(int(run.ID.Version())).To(Equal(7))
			if previous != uuid.Nil {
				Expect(bytes.Compare(run.ID[:], previous[:])).To(BeNumerically(">", 0),
					"id %s is not after previous %s", run.ID, previous)
			}
			previous = run.ID
		}
	})

	It("keeps an explicitly set ID", func() {
		fixed := uuid.New()
		run := AnsibleRun{ID: fixed}
		Expect(run.BeforeCreate(&gorm.DB{})).To(Succeed())
		Expect(run.ID).To(Equal(fixed))
	})
})
