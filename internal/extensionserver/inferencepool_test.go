// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extensionserver

import (
	"fmt"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gwaiev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	"github.com/envoyproxy/ai-gateway/internal/internalapi"
)

// TestBuildClustersForInferencePoolEndpointPickers_Deduplicates verifies that multiple
// route references to the same InferencePool produce exactly one EPP cluster per pool,
// and that conflicting references resolve deterministically (PLA-7735).
func TestBuildClustersForInferencePoolEndpointPickers_Deduplicates(t *testing.T) {
	mkCluster := func(poolRef string) *clusterv3.Cluster {
		return &clusterv3.Cluster{
			Metadata: &corev3.Metadata{
				FilterMetadata: map[string]*structpb.Struct{
					internalapi.InternalEndpointMetadataNamespace: {
						Fields: map[string]*structpb.Value{
							"per_route_rule_inference_pool": structpb.NewStringValue(poolRef),
						},
					},
				},
			},
		}
	}

	poolNames := []string{
		"corti-s1-20261017",
		"corti-s1-20261026",
		"corti-s1-mini-20260423",
		"corti-s1-mini-20260831",
	}

	// Two route references per pool, identical metadata. Without deduplication this
	// produced 8 clusters (one per route reference) for the 4 pools on prd-par02.
	var clusters []*clusterv3.Cluster
	for _, poolName := range poolNames {
		ref := fmt.Sprintf("mlservices/%s/%s-epp/9002/duplex/false", poolName, poolName)
		clusters = append(clusters, mkCluster(ref), mkCluster(ref))
	}
	result, err := buildClustersForInferencePoolEndpointPickers(collectInferencePools(logr.Discard(), clusters, nil))
	require.NoError(t, err)
	var names []string
	for _, c := range result {
		names = append(names, c.Name)
	}
	require.Equal(t, []string{
		"envoy.clusters.endpointpicker_corti-s1-20261017_mlservices_ext_proc",
		"envoy.clusters.endpointpicker_corti-s1-20261026_mlservices_ext_proc",
		"envoy.clusters.endpointpicker_corti-s1-mini-20260423_mlservices_ext_proc",
		"envoy.clusters.endpointpicker_corti-s1-mini-20260831_mlservices_ext_proc",
	}, names)

	// Conflicting references to the same pool (e.g. EPP service changed mid-reconcile):
	// the first reference in the list wins, deterministically, in both input orders.
	refA := mkCluster("mlservices/corti-s1-20261017/epp-a/9002/duplex/false")
	refB := mkCluster("mlservices/corti-s1-20261017/epp-b/9090/duplex/false")
	orders := [][]*clusterv3.Cluster{{refA, refB}, {refB, refA}}
	for _, input := range orders {
		for range 25 {
			result, err = buildClustersForInferencePoolEndpointPickers(collectInferencePools(logr.Discard(), input, nil))
			require.NoError(t, err)
			require.Len(t, result, 1)
			require.Equal(t, "envoy.clusters.endpointpicker_corti-s1-20261017_mlservices_ext_proc", result[0].Name)
			addr := result[0].GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].
				GetEndpoint().GetAddress().GetSocketAddress().GetAddress()
			wantAddr := "epp-a.mlservices.svc"
			if input[0] == refB {
				wantAddr = "epp-b.mlservices.svc"
			}
			require.Equal(t, wantAddr, addr)
		}
	}
}

// TestPortForInferencePool_EdgeCases covers edge cases for port selection
// in portForInferencePool, such as invalid or out-of-range ports.
func TestPortForInferencePool_EdgeCases(t *testing.T) {
	pool := &gwaiev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pool",
			Namespace: "my-ns",
		},
		Spec: gwaiev1.InferencePoolSpec{
			EndpointPickerRef: gwaiev1.EndpointPickerRef{
				Name: "my-picker",
				Port: ptr.To(gwaiev1.Port{Number: 8080}),
			},
		},
	}

	// Test invalid port (should fallback to default)
	poolInvalidPort := pool.DeepCopy()
	poolInvalidPort.Spec.EndpointPickerRef.Port = &gwaiev1.Port{Number: 70000} // > 65535
	assert.Equal(t, uint32(defaultEndpointPickerPort), portForInferencePool(poolInvalidPort))

	poolNegativePort := pool.DeepCopy()
	poolNegativePort.Spec.EndpointPickerRef.Port = &gwaiev1.Port{Number: -1} // < 0 (though type is usually uint, check logic)
	// Note: gwaiev1.PortNumber is int32, so negative is possible in struct but logic handles it
	assert.Equal(t, uint32(defaultEndpointPickerPort), portForInferencePool(poolNegativePort))
}

// TestBuildAndParseMetadata_RoundTrip ensures the metadata encoding/decoding remains consistent.
// This serves as an integration check between buildEPPMetadata and getInferencePoolByMetadata.
func TestBuildAndParseMetadata_RoundTrip(t *testing.T) {
	pool := &gwaiev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "test-ns",
			Annotations: map[string]string{
				processingBodyModeAnnotation: "buffered",
				allowModeOverrideAnnotation:  "true",
			},
		},
		Spec: gwaiev1.InferencePoolSpec{
			EndpointPickerRef: gwaiev1.EndpointPickerRef{
				Name: "test-picker",
				Port: ptr.To(gwaiev1.Port{Number: 9090}),
			},
		},
	}

	// 1. Build Metadata
	metadata := &corev3.Metadata{}
	buildEPPMetadata(metadata, pool)

	// Verify metadata structure
	filterMeta, ok := metadata.FilterMetadata[internalapi.InternalEndpointMetadataNamespace]
	assert.True(t, ok)
	assert.NotNil(t, filterMeta)

	val, ok := filterMeta.Fields[internalMetadataInferencePoolKey]
	assert.True(t, ok)
	encodedStr := val.GetStringValue()

	// Check encoded format: ns/name/svc/port/mode/override
	assert.Equal(t, "test-ns/test-pool/test-picker/9090/buffered/true", encodedStr)

	// 2. Parse Metadata back to Pool
	parsedPool := getInferencePoolByMetadata(metadata)
	assert.NotNil(t, parsedPool)

	// Verify restored properties
	assert.Equal(t, "test-pool", parsedPool.Name)
	assert.Equal(t, "test-ns", parsedPool.Namespace)
	assert.Equal(t, "test-picker", string(parsedPool.Spec.EndpointPickerRef.Name))
	assert.Equal(t, gwaiev1.PortNumber(9090), parsedPool.Spec.EndpointPickerRef.Port.Number)

	// Verify restored annotations
	extractedMode := parsedPool.Annotations[processingBodyModeAnnotation]
	extractedOverride := parsedPool.Annotations[allowModeOverrideAnnotation]
	assert.Equal(t, "buffered", extractedMode)
	assert.Equal(t, "true", extractedOverride)
}

// TestGetInferencePoolByMetadata_Malformed tests parsing of invalid metadata strings.
func TestGetInferencePoolByMetadata_Malformed(t *testing.T) {
	// Test nil metadata
	assert.Nil(t, getInferencePoolByMetadata(nil))

	// Test missing namespace
	md := &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{}}
	assert.Nil(t, getInferencePoolByMetadata(md))

	// Test invalid format string (not enough parts)
	md = &corev3.Metadata{
		FilterMetadata: map[string]*structpb.Struct{
			internalapi.InternalEndpointMetadataNamespace: {
				Fields: map[string]*structpb.Value{
					internalMetadataInferencePoolKey: structpb.NewStringValue("invalid/format/string"),
				},
			},
		},
	}
	assert.Nil(t, getInferencePoolByMetadata(md))

	// Test invalid port component
	md = &corev3.Metadata{
		FilterMetadata: map[string]*structpb.Struct{
			internalapi.InternalEndpointMetadataNamespace: {
				Fields: map[string]*structpb.Value{
					internalMetadataInferencePoolKey: structpb.NewStringValue("ns/name/svc/not-a-port/duplex/false"),
				},
			},
		},
	}
	assert.Nil(t, getInferencePoolByMetadata(md))
}

// TestBuildHTTPFilterForInferencePool_Defaults verifies default behavior separate from annotation parsing.
func TestBuildHTTPFilterForInferencePool_Defaults(t *testing.T) {
	pool := &gwaiev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "defaults-pool",
			Namespace: "default",
		},
		Spec: gwaiev1.InferencePoolSpec{
			EndpointPickerRef: gwaiev1.EndpointPickerRef{Name: "default-picker"},
		},
	}

	filter := buildHTTPFilterForInferencePool(pool)
	assert.NotNil(t, filter)
	// Expect duplex by default
	assert.Equal(t, extprocv3.ProcessingMode_FULL_DUPLEX_STREAMED, filter.ProcessingMode.RequestBodyMode)
	assert.Equal(t, extprocv3.ProcessingMode_FULL_DUPLEX_STREAMED, filter.ProcessingMode.ResponseBodyMode)
	assert.False(t, filter.AllowModeOverride)
}
