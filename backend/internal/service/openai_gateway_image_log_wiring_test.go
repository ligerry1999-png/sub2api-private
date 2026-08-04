package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestProvideOpenAIGatewayServiceRejectsMissingImageLogDependency(t *testing.T) {
	service, err := provideOpenAIGatewayServiceForImageLogTest(nil)

	require.Nil(t, service)
	require.ErrorContains(t, err, "image log service is not connected")
}

func TestProvideOpenAIGatewayServiceConnectsImageLogDependency(t *testing.T) {
	imageLogService := &ImageLogService{}

	service, err := provideOpenAIGatewayServiceForImageLogTest(imageLogService)

	require.NoError(t, err)
	require.NotNil(t, service)
	require.Same(t, imageLogService, service.imageLogService)
	require.NoError(t, service.validateImageLogDependency())
}

func provideOpenAIGatewayServiceForImageLogTest(imageLogService *ImageLogService) (*OpenAIGatewayService, error) {
	return ProvideOpenAIGatewayService(
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		&config.Config{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		imageLogService,
	)
}
