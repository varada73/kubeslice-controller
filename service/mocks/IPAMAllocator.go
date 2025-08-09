// Code generated manually for testing; DO NOT USE in production without review.S
package mocks

import (
	"context"

	"github.com/stretchr/testify/mock"
)

// IPAMAllocator is a testify mock for the IPAMAllocator interface.
type IPAMAllocator struct {
	mock.Mock
}

// InitializePool mocks the InitializePool method.
func (_m *IPAMAllocator) InitializePool(sliceName, sliceSubnet string) error {
	ret := _m.Called(sliceName, sliceSubnet)
	return ret.Error(0)
}

// Allocate mocks the Allocate method.
func (_m *IPAMAllocator) Allocate(ctx context.Context, sliceName string, clusterName string, requiredCIDRSize int) (string, error) {
	ret := _m.Called(ctx, sliceName, clusterName, requiredCIDRSize)

	var r0 string
	if rf, ok := ret.Get(0).(func(context.Context, string, string, int) string); ok {
		r0 = rf(ctx, sliceName, clusterName, requiredCIDRSize)
	} else {
		r0 = ret.String(0)
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, string, string, int) error); ok {
		r1 = rf(ctx, sliceName, clusterName, requiredCIDRSize)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// Reclaim mocks the Reclaim method.
func (_m *IPAMAllocator) Reclaim(ctx context.Context, sliceName string, clusterName string) error {
	ret := _m.Called(ctx, sliceName, clusterName)
	return ret.Error(0)
}
