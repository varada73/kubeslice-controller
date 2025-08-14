package service

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/dailymotion/allure-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDynamicIPAMAllocateSuite(t *testing.T) {
	for k, v := range IPAMAllocateTestBed {
		t.Run(k, func(t *testing.T) {
			allure.Test(t, allure.Name(k),
				allure.Action(func() {
					v(t)
				}))
		})
	}
}

var IPAMAllocateTestBed = map[string]func(*testing.T){
	"TestDynamicIPAMAllocator_InitializePool": TestDynamicIPAMAllocator_InitializePool,
	"TestDynamicIPAMAllocator_Allocate":       TestDynamicIPAMAllocator_Allocate,
	"TestDynamicIPAMAllocator_Reclaim":        TestDynamicIPAMAllocator_Reclaim,
	"TestHelperFunctions":                     TestHelperFunctions,
}

func TestDynamicIPAMAllocator_InitializePool(t *testing.T) {
	allocator := NewDynamicIPAMAllocator()
	sliceName := "test-slice"
	sliceSubnet := "10.0.0.0/16"

	t.Run("Successfully initialize pool", func(t *testing.T) {
		err := allocator.InitializePool(sliceName, sliceSubnet)
		require.NoError(t, err)
		vpnSubnet, err := allocator.Allocate(context.Background(), sliceName, "VPN_Subnet", 24)
		require.NoError(t, err)
		assert.NotEmpty(t, vpnSubnet)
		t.Logf("VPN subnet reserved: %s", vpnSubnet)
	})

	t.Run("Idempotent re-initialization", func(t *testing.T) {
		err := allocator.InitializePool(sliceName, sliceSubnet)
		require.NoError(t, err, "re-initializing an existing pool should not return an error")
	})

	t.Run("Invalid slice subnet CIDR", func(t *testing.T) {
		err := allocator.InitializePool("invalid-slice", "192.168.1.0/33")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid slice subnet CIDR")
	})
}

func TestDynamicIPAMAllocator_Allocate(t *testing.T) {
	allocator := NewDynamicIPAMAllocator()
	sliceName := "dev-slice"
	sliceSubnet := "10.10.0.0/16"

	err := allocator.InitializePool(sliceName, sliceSubnet)
	require.NoError(t, err)

	t.Run("Successful initial allocation", func(t *testing.T) {

		clusterName := "cluster-a"
		requiredCIDRSize := 24
		allocatedCIDR, err := allocator.Allocate(context.Background(), sliceName, clusterName, requiredCIDRSize)
		require.NoError(t, err)
		assert.NotEmpty(t, allocatedCIDR)
		t.Logf("Allocated %s for %s/%s", allocatedCIDR, sliceName, clusterName)

		_, ipNet, err := net.ParseCIDR(allocatedCIDR)
		require.NoError(t, err)
		bits, _ := ipNet.Mask.Size()
		assert.Equal(t, requiredCIDRSize, bits)
	})

	t.Run("Idempotent allocation for existing cluster", func(t *testing.T) {
		clusterName := "cluster-a"
		requiredCIDRSize := 24

		allocatedCIDR, err := allocator.Allocate(context.Background(), sliceName, clusterName, requiredCIDRSize)
		require.NoError(t, err)
		assert.NotEmpty(t, allocatedCIDR)
		t.Logf("Re-allocated (idempotent) %s for %s/%s", allocatedCIDR, sliceName, clusterName)
	})

	t.Run("Allocation for a new cluster", func(t *testing.T) {
		clusterName := "cluster-b"
		requiredCIDRSize := 24
		allocatedCIDR, err := allocator.Allocate(context.Background(), sliceName, clusterName, requiredCIDRSize)
		require.NoError(t, err)
		assert.NotEmpty(t, allocatedCIDR)
		t.Logf("Allocated %s for %s/%s", allocatedCIDR, sliceName, clusterName)
	})

	t.Run("Allocation for different CIDR size (should error with current implementation)", func(t *testing.T) {
		clusterName := "cluster-b"
		requiredCIDRSize := 25
		_, err := allocator.Allocate(context.Background(), sliceName, clusterName, requiredCIDRSize)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Re-allocation not supported in this version.")
	})

	t.Run("Allocation when no suitable free block is available", func(t *testing.T) {

		smallAllocator := NewDynamicIPAMAllocator()
		smallSliceName := "small-slice"
		smallSliceSubnet := "172.16.0.0/20"
		err = smallAllocator.InitializePool(smallSliceName, smallSliceSubnet)
		require.NoError(t, err)

		for i := 0; i < 15; i++ {
			_, err = smallAllocator.Allocate(context.Background(), smallSliceName, fmt.Sprintf("exhaust-cluster-%d", i), 24)
			require.NoError(t, err)
		}

		_, err = smallAllocator.Allocate(context.Background(), smallSliceName, "big-cluster", 23)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no available subnet of size /23")

		_, err = smallAllocator.Allocate(context.Background(), smallSliceName, "last-cluster", 24)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no available subnet of size /24")
	})

	t.Run("Allocate for uninitialized slice", func(t *testing.T) {
		_, err := allocator.Allocate(context.Background(), "non-existent-slice", "some-cluster", 24)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ipam pool for slice non-existent-slice is not initialized")
	})

	t.Run("Multiple allocations and splitting", func(t *testing.T) {
		fmt.Printf("\n\n\n\n MULTIPLE ALLOCATIONS AND SPLITTING\n")
		multiAllocator := NewDynamicIPAMAllocator()
		multiSliceName := "multi-slice"
		multiSliceSubnet := "192.168.0.0/16"
		err := multiAllocator.InitializePool(multiSliceName, multiSliceSubnet)
		require.NoError(t, err)

		cidr1, err := multiAllocator.Allocate(context.Background(), multiSliceName, "multi-cluster-1", 24)
		require.NoError(t, err)
		assert.NotEmpty(t, cidr1)

		cidr2, err := multiAllocator.Allocate(context.Background(), multiSliceName, "multi-cluster-2", 24)
		require.NoError(t, err)
		assert.NotEmpty(t, cidr2)
		assert.NotEqual(t, cidr1, cidr2, "allocated CIDRs should be different")

		cidr3, err := multiAllocator.Allocate(context.Background(), multiSliceName, "multi-cluster-3", 20)
		require.NoError(t, err)
		assert.NotEmpty(t, cidr3)
		_, ipNet3, _ := net.ParseCIDR(cidr3)
		bits3, _ := ipNet3.Mask.Size()
		assert.Equal(t, 20, bits3)

		multiSliceName1 := "multi-slice1"
		multiSliceSubnet1 := "192.160.0.0/18"
		err = multiAllocator.InitializePool(multiSliceName1, multiSliceSubnet1)
		require.NoError(t, err)

		cidr4, err := multiAllocator.Allocate(context.Background(), multiSliceName1, "multi-cluster-4", 21)
		require.NoError(t, err)
		assert.NotEmpty(t, cidr4)
		_, ipNet4, _ := net.ParseCIDR(cidr4)
		bits4, _ := ipNet4.Mask.Size()
		assert.Equal(t, 21, bits4)
		t.Logf("Allocated %s (/%d) for %s/%s", cidr3, bits3, multiSliceName, "multi-cluster-3")
	})
}

func TestDynamicIPAMAllocator_Reclaim(t *testing.T) {
	allocator := NewDynamicIPAMAllocator()
	sliceName := "prod-slice"
	sliceSubnet := "10.20.0.0/16"

	err := allocator.InitializePool(sliceName, sliceSubnet)
	require.NoError(t, err)

	_, err = allocator.Allocate(context.Background(), sliceName, "app-cluster-1", 24)
	require.NoError(t, err)
	_, err = allocator.Allocate(context.Background(), sliceName, "app-cluster-2", 24)
	require.NoError(t, err)
	_, err = allocator.Allocate(context.Background(), sliceName, "app-cluster-3", 24)
	require.NoError(t, err)

	t.Run("Successfully reclaim a subnet", func(t *testing.T) {
		clusterName := "app-cluster-1"
		err := allocator.Reclaim(context.Background(), sliceName, clusterName)
		require.NoError(t, err)

		allocatedCIDR, err := allocator.Allocate(context.Background(), sliceName, clusterName, 24)
		require.NoError(t, err)
		assert.NotEmpty(t, allocatedCIDR, "reclaimed subnet should be available for re-allocation")
		t.Logf("Reclaimed and re-allocated %s for %s/%s", allocatedCIDR, sliceName, clusterName)
	})

	t.Run("Reclaim non-existent allocation", func(t *testing.T) {
		err := allocator.Reclaim(context.Background(), sliceName, "non-existent-cluster")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no allocated subnet")
	})

	t.Run("Reclaim from uninitialized slice", func(t *testing.T) {
		err := allocator.Reclaim(context.Background(), "another-slice", "some-cluster")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ipam pool for slice another-slice is not initialized")
	})

	t.Run("Reclaim and merge adjacent blocks", func(t *testing.T) {
		mergeAllocator := NewDynamicIPAMAllocator()
		mergeSliceName := "merge-slice"
		mergeSliceSubnet := "10.30.0.0/23"
		err := mergeAllocator.InitializePool(mergeSliceName, mergeSliceSubnet)
		require.NoError(t, err)

		mergeAllocator = NewDynamicIPAMAllocator()
		err = mergeAllocator.InitializePool(mergeSliceName, mergeSliceSubnet)
		require.NoError(t, err)

		clusterXSubnet, err := mergeAllocator.Allocate(context.Background(), mergeSliceName, "cluster-X", 25)
		require.NoError(t, err)
		t.Logf("Allocated %s for cluster-X", clusterXSubnet)

		clusterYSubnet, err := mergeAllocator.Allocate(context.Background(), mergeSliceName, "cluster-Y", 25)
		require.NoError(t, err)
		t.Logf("Allocated %s for cluster-Y", clusterYSubnet)

		err = mergeAllocator.Reclaim(context.Background(), mergeSliceName, "cluster-X")
		require.NoError(t, err)

		err = mergeAllocator.Reclaim(context.Background(), mergeSliceName, "cluster-Y")
		require.NoError(t, err)

		mergedAlloc, err := mergeAllocator.Allocate(context.Background(), mergeSliceName, "merged-cluster", 24)
		require.NoError(t, err, "expected /24 to be available after merging two /25s")
		assert.NotEmpty(t, mergedAlloc)
		_, mergedNet, _ := net.ParseCIDR(mergedAlloc)
		bits, _ := mergedNet.Mask.Size()
		assert.Equal(t, 24, bits, "expected a /24 to be allocated after merge")
		t.Logf("Allocated %s (/%d) for merged-cluster, confirming merge", mergedAlloc, bits)
	})
}

func TestHelperFunctions(t *testing.T) {
	t.Run("compareIPs", func(t *testing.T) {
		ip1 := net.ParseIP("192.168.1.1")
		ip2 := net.ParseIP("192.168.1.10")
		ip3 := net.ParseIP("192.168.1.1")
		ipV6_1 := net.ParseIP("::1")
		ipV6_2 := net.ParseIP("::10")

		assert.Equal(t, -1, compareIPs(ip1, ip2))
		assert.Equal(t, 1, compareIPs(ip2, ip1))
		assert.Equal(t, 0, compareIPs(ip1, ip3))

		assert.Equal(t, -1, compareIPs(ipV6_1, ipV6_2))
		assert.Equal(t, 1, compareIPs(ipV6_2, ipV6_1))
		assert.Equal(t, -1, compareIPs(ip1, ipV6_1), "IPv4 should be 'smaller' than IPv6")
		assert.Equal(t, 1, compareIPs(ipV6_1, ip1), "IPv6 should be 'larger' than IPv4")
	})

	t.Run("compareIPNets", func(t *testing.T) {
		_, net1, _ := net.ParseCIDR("192.168.1.0/24")
		_, net2, _ := net.ParseCIDR("192.168.2.0/24")
		_, net3, _ := net.ParseCIDR("192.168.1.0/25")
		_, net4, _ := net.ParseCIDR("192.168.1.0/24")

		assert.Equal(t, -1, compareIPNets(net1, net2))
		assert.Equal(t, 1, compareIPNets(net2, net1))
		assert.Equal(t, -1, compareIPNets(net3, net1), "192.168.1.0/25 should come before 192.168.1.0/24 if sorted by mask size after IP")
		fmt.Printf("net1: %s, net3: %s, Compare: %d\n", net3.String(), net1.String(), compareIPNets(net3, net1))
		assert.Equal(t, 1, compareIPNets(net1, net3))
		fmt.Printf("net3: %s, net1: %s, Compare: %d\n", net1.String(), net3.String(), compareIPNets(net1, net3))
		assert.Equal(t, 0, compareIPNets(net1, net4))
	})

	t.Run("incIP", func(t *testing.T) {
		ip := net.ParseIP("192.168.1.254").To4()
		ip = incIP(ip, 1)
		assert.Equal(t, "192.168.1.255", ip.String())

		ip = net.ParseIP("192.168.1.255").To4()
		ip = incIP(ip, 1)
		assert.Equal(t, "192.168.2.0", ip.String())

		ip = net.ParseIP("10.0.0.0").To4()
		ip = incIP(ip, 256)
		assert.Equal(t, "10.0.1.0", ip.String())

		ip = net.ParseIP("10.0.0.0").To4()
		ip = incIP(ip, 1<<uint(32-20))
		assert.Equal(t, "10.0.16.0", ip.String())
	})

	t.Run("tryMerge", func(t *testing.T) {
		_, net1, _ := net.ParseCIDR("192.168.1.0/24")
		_, net2, _ := net.ParseCIDR("192.168.2.0/25")
		_, net3, _ := net.ParseCIDR("192.168.2.0/24")

		merged, ok := tryMerge(net1, net2)
		assert.False(t, ok)
		assert.Nil(t, merged)

		merged, ok = tryMerge(net1, net3)
		assert.True(t, ok)
		assert.NotNil(t, merged)
		assert.Equal(t, "192.168.1.0/23", merged.String())

		_, blockA, _ := net.ParseCIDR("192.168.1.0/25")
		_, blockB, _ := net.ParseCIDR("192.168.1.128/25")
		merged, ok = tryMerge(blockA, blockB)

		assert.True(t, ok)
		assert.NotNil(t, merged)
		assert.Equal(t, "192.168.1.0/24", merged.String())

		merged, ok = tryMerge(blockB, blockA)
		assert.False(t, ok, "tryMerge expects blocks in sorted order for simpler logic, otherwise order check is needed")

	})
}
