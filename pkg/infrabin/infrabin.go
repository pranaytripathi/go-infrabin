package infrabin

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/maruina/go-infrabin/internal/aws"
	"github.com/maruina/go-infrabin/internal/helpers"
	"github.com/spf13/viper"
)

// Must embed UnimplementedInfrabinServer for `protogen-gen-go-grpc`
type InfrabinService struct {
	UnimplementedInfrabinServer
	STSClient                 aws.STSApi
	IntermittentErrorsCounter int32
}

func (s *InfrabinService) Root(ctx context.Context, _ *Empty) (*Response, error) {
	fail := helpers.GetEnv("FAIL_ROOT_HANDLER", "")
	if fail != "" {
		return nil, status.Error(codes.Unavailable, "some description")
	} else {
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatalf("cannot get hostname: %v", err)
		}

		var resp Response
		resp.Hostname = hostname
		// Take kubernetes info from a couple of _common_ environment variables
		resp.Kubernetes = &KubeResponse{
			PodName:     helpers.GetEnv("POD_NAME", "K8S_POD_NAME", ""),
			Namespace:   helpers.GetEnv("POD_NAMESPACE", "K8S_NAMESPACE", ""),
			PodIp:       helpers.GetEnv("POD_IP", "K8S_POD_IP", ""),
			NodeName:    helpers.GetEnv("NODE_NAME", "K8S_NODE_NAME", ""),
			ClusterName: helpers.GetEnv("CLUSTER_NAME", "K8S_CLUSTER_NAME", ""),
			Region:      helpers.GetEnv("REGION", "AWS_REGION", "FUNCTION_REGION", ""),
		}
		return &resp, nil
	}
}

func (s *InfrabinService) Delay(ctx context.Context, request *DelayRequest) (*Response, error) {
	maxDelay := viper.GetDuration("maxDelay")
	requestDuration := time.Duration(request.Duration) * time.Second

	duration := helpers.MinDuration(requestDuration, maxDelay)
	time.Sleep(duration)

	return &Response{Delay: int32(duration.Seconds())}, nil
}

func (s *InfrabinService) Env(ctx context.Context, request *EnvRequest) (*Response, error) {
	value := helpers.GetEnv(request.EnvVar, "")
	if value == "" {
		return nil, status.Errorf(codes.NotFound, "No env var named %s", request.EnvVar)
	} else {
		return &Response{Env: map[string]string{request.EnvVar: value}}, nil
	}
}

func (s *InfrabinService) Headers(ctx context.Context, request *HeadersRequest) (*Response, error) {
	if request.Headers == nil {
		request.Headers = make(map[string]string)
	}
	md, _ := metadata.FromIncomingContext(ctx)
	for key := range md {
		request.Headers[key] = strings.Join(md.Get(key), ",")
	}
	return &Response{Headers: request.Headers}, nil
}

func (s *InfrabinService) Proxy(ctx context.Context, request *ProxyRequest) (*structpb.Struct, error) {
	if !viper.GetBool("proxyEndpoint") {
		return nil, status.Errorf(codes.Unimplemented, "Proxy endpoint disabled. Enabled with --enable-proxy-endpoint")
	}

	// Compile the regexp
	exp := viper.GetString("proxyAllowRegexp")
	r, err := regexp.Compile(exp)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Unable to compile %s regexp: %v", exp, err)
	}

	// Convert Struct into json []byte
	requestBody, err := request.Body.MarshalJSON()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Unable to marshal downstream request body: %v", err)
	}

	// Check if the target URL is allowed
	if !r.MatchString(request.Url) {
		return nil, status.Errorf(codes.InvalidArgument, "Unable to build request as the target URL %s is blocked by the regexp %s", request.Url, exp)
	}

	// Make upstream request from incoming request
	req, err := http.NewRequestWithContext(ctx, request.Method, request.Url, bytes.NewReader(requestBody))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Unable to build request: %v", err)
	}
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}

	// Send http request
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Unable to reach %s: %v", request.Url, err)
	}

	// Read request body and close it
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Error reading upstream response body: %v", err)
	}
	if err = resp.Body.Close(); err != nil {
		return nil, status.Errorf(codes.Internal, "Error closing upstream response: %v", err)
	}

	// Convert []bytes into json struct
	var response structpb.Struct
	if err := response.UnmarshalJSON(body); err != nil {
		return nil, status.Errorf(codes.Internal, "Error creating Struct from upstream response json: %v", err)
	}
	return &response, nil
}

func (s *InfrabinService) AWSMetadata(ctx context.Context, request *AWSMetadataRequest) (*structpb.Struct, error) {
	if request.Path == "" {
		return nil, status.Errorf(codes.InvalidArgument, "path must not be empty")
	}

	u, err := url.Parse(viper.GetString("awsMetadataEndpoint"))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "s.Config.AWSMetadataEndpoint invalid: %v", err)
	}

	u.Path = request.Path
	return s.Proxy(ctx, &ProxyRequest{Method: "GET", Url: u.String()})
}

func (s *InfrabinService) Any(ctx context.Context, request *AnyRequest) (*Response, error) {
	return &Response{Path: request.Path}, nil
}

func (s *InfrabinService) AWSAssume(ctx context.Context, request *AWSAssumeRequest) (*Response, error) {
	if request.Role == "" {
		return nil, status.Errorf(codes.InvalidArgument, "role must not be empty")
	}

	roleId, err := aws.STSAssumeRole(ctx, s.STSClient, request.Role, "aws-assume-session-go-infrabin")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Error assuming AWS IAM role, %v", err)
	}

	return &Response{AssumedRoleId: roleId}, nil
}

func (s *InfrabinService) AWSGetCallerIdentity(ctx context.Context, _ *Empty) (*Response, error) {
	response, err := aws.STSGetCallerIdentity(ctx, s.STSClient)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Error calling AWS Get Caller Identity, %v", err)
	}

	return &Response{
		GetCallerIdentity: &GetCallerIdentityResponse{
			Account: *response.Account,
			Arn:     *response.Arn,
			UserId:  *response.UserId,
		},
	}, nil
}

func (s *InfrabinService) Intermittent(ctx context.Context, _ *Empty) (*Response, error) {
	maxErrs := viper.GetInt32("intermittentErrors")

	if s.IntermittentErrorsCounter < maxErrs {
		s.IntermittentErrorsCounter++
		return nil, status.Errorf(codes.Unavailable, fmt.Sprintf("%d errors left", maxErrs-s.IntermittentErrorsCounter+1))
	}

	s.IntermittentErrorsCounter = 0
	return &Response{
		Intermittent: &IntermittentResponse{
			IntermittentErrors: maxErrs,
		},
	}, nil
}
