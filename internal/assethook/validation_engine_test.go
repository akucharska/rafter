package assethook_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/kyma-project/rafter/internal/assethook"
	"github.com/kyma-project/rafter/internal/assethook/automock"
	"github.com/kyma-project/rafter/pkg/apis/rafter/v1beta1"
	"github.com/onsi/gomega"
)

func TestValidationEngine_Validate(t *testing.T) {
	for testName, testCase := range map[string]struct {
		err      error
		messages map[string][]assethook.Message
	}{
		"success": {},
		"error": {
			err: fmt.Errorf("test"),
		},
		"fail": {
			messages: map[string][]assethook.Message{
				"test": {
					{Filename: "test", Message: "test"},
				},
			},
		},
	} {
		t.Run(testName, func(t *testing.T) {
			// Given
			g := gomega.NewGomegaWithT(t)

			processor := automock.NewHttpProcessor()
			defer processor.AssertExpectations(t)
			ctx := context.TODO()
			files := []string{}
			services := []v1beta1.AssetWebhookService{}

			processor.On("Do", ctx, "", files, services).Return(testCase.messages, testCase.err).Once()
			validator := assethook.NewTestValidator(processor)

			// When
			result, err := validator.Validate(ctx, "", files, services)

			// Then
			if testCase.err == nil {
				g.Expect(err).ToNot(gomega.HaveOccurred())
			} else {
				g.Expect(err).To(gomega.HaveOccurred())
			}

			if len(testCase.messages) == 0 && err == nil {
				g.Expect(result.Success).To(gomega.BeTrue())
			} else {
				g.Expect(result.Success).To(gomega.BeFalse())
			}
		})
	}
}
