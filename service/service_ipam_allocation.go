package service

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
)

type IPAMAllocator interface {
	InitializePool(sliceName, sliceSubnet string) error
	Allocate(ctx context.Context, sliceName string, clusterName string, requiredCIDRSize int) (string, error)
	Reclaim(ctx context.Context, sliceName string, clusterName string) error
}

// sliceIPPool holds the state for a single slice's IPAM.
type sliceIPPool struct {
	SliceSubnet *net.IPNet
	// Mutex to protect concurrent access to this pool's state.
	mu         sync.Mutex
	Allocated  map[string]*net.IPNet
	FreeBlocks []*net.IPNet
}

type DynamicIPAMAllocator struct {
	mu    sync.Mutex
	pools map[string]*sliceIPPool
}

func NewDynamicIPAMAllocator() *DynamicIPAMAllocator {
	return &DynamicIPAMAllocator{
		pools: make(map[string]*sliceIPPool),
	}
}

func (a *DynamicIPAMAllocator) InitializePool(sliceName, sliceSubnetStr string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.pools[sliceName]; exists {
		return nil
	}

	_, sliceNet, err := net.ParseCIDR(sliceSubnetStr)
	if err != nil {
		return fmt.Errorf("invalid slice subnet CIDR: %w", err)
	}

	pool := &sliceIPPool{
		SliceSubnet: sliceNet,
		Allocated:   make(map[string]*net.IPNet),
		FreeBlocks:  []*net.IPNet{sliceNet}, // Initially, the entire slice subnet is free
	}

	a.pools[sliceName] = pool
	fmt.Printf("InitializePool: After creation, pool.Allocated for %s: %v\n", sliceName, pool.Allocated)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	//Allocation if subnet for VPN is required for each slice even if it is not a cluster in the slice.
	vpnSubnetRequiredSize := 24
	vpnClusterName := "VPN_Subnet"

	_, err = pool.allocateSubnetForPool(vpnClusterName, vpnSubnetRequiredSize)
	if err != nil {
		return fmt.Errorf("failed to reserve VPN subnet for slice %s: %w", sliceName, err)
	}

	return nil
}

// Allocate allocates a subnet for a specific cluster within a slice.
func (a *DynamicIPAMAllocator) Allocate(ctx context.Context, sliceName string, clusterName string, requiredCIDRSize int) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, exists := a.pools[sliceName]
	if !exists {
		return "", fmt.Errorf("ipam pool for slice %s is not initialized", sliceName)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	allocatedNet, err := pool.allocateSubnetForPool(clusterName, requiredCIDRSize)
	if err != nil {
		return "", fmt.Errorf("failed to allocate subnet for cluster %s in slice %s: %w", clusterName, sliceName, err)
	}

	return allocatedNet.String(), nil
}

// It attempts to merge the reclaimed block with adjacent free blocks to reduce fragmentation.
func (a *DynamicIPAMAllocator) Reclaim(ctx context.Context, sliceName string, clusterName string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, exists := a.pools[sliceName]
	if !exists {
		return fmt.Errorf("ipam pool for slice %s is not initialized", sliceName)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	subnetToReclaim, allocated := pool.Allocated[clusterName]
	if !allocated {
		return fmt.Errorf("cluster %s has no allocated subnet in slice %s to reclaim", clusterName, sliceName)
	}

	delete(pool.Allocated, clusterName)

	pool.FreeBlocks = append(pool.FreeBlocks, subnetToReclaim)

	sort.Slice(pool.FreeBlocks, func(i, j int) bool {
		return compareIPNets(pool.FreeBlocks[i], pool.FreeBlocks[j]) < 0
	})

	newFreeBlocks := []*net.IPNet{}
	if len(pool.FreeBlocks) > 0 {
		current := pool.FreeBlocks[0]
		for i := 1; i < len(pool.FreeBlocks); i++ {
			next := pool.FreeBlocks[i]
			merged, ok := tryMerge(current, next)
			if ok {
				current = merged // Successfully merged, continue with the larger block
			} else {
				newFreeBlocks = append(newFreeBlocks, current) // No merge, add current and move to next
				current = next
			}
		}
		newFreeBlocks = append(newFreeBlocks, current) // Add the last (or unmerged) block
	}
	pool.FreeBlocks = newFreeBlocks

	return nil
}

// --- Helper Functions for IPNet Manipulation ---

func copyIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	out := make(net.IP, len(ip))
	copy(out, ip)
	return out
}
func (pool *sliceIPPool) allocateSubnetForPool(clusterName string, requiredCIDRSize int) (*net.IPNet, error) {

	if allocatedNet, found := pool.Allocated[clusterName]; found {
		ones, _ := allocatedNet.Mask.Size()
		existingBits := ones
		if existingBits == requiredCIDRSize {
			return allocatedNet, nil
		}

		return nil, fmt.Errorf("cluster %s already has subnet %s (/%d), but requested /%d. Re-allocation not supported in this version.",
			clusterName, allocatedNet.String(), existingBits, requiredCIDRSize)
	}

	var firstFitIndex = -1
	var firstFitNet *net.IPNet

	for i, freeNet := range pool.FreeBlocks {
		ones, _ := freeNet.Mask.Size()
		freeBits := ones
		if freeBits <= requiredCIDRSize {
			firstFitIndex = i
			ipCopy := copyIP(freeNet.IP)
			maskCopy := append(net.IPMask(nil), freeNet.Mask...)
			firstFitNet = &net.IPNet{IP: ipCopy, Mask: maskCopy}
			break
		}
	}

	if firstFitIndex == -1 {
		return nil, fmt.Errorf("no available subnet of size /%d in pool", requiredCIDRSize)
	}

	ones, _ := firstFitNet.Mask.Size()
	firstFitBits := ones

	var allocatedNet *net.IPNet
	remainderNets := []*net.IPNet{}

	if firstFitBits < requiredCIDRSize {

		startIP := copyIP(firstFitNet.IP)
		allocatedNet = &net.IPNet{IP: startIP, Mask: net.CIDRMask(requiredCIDRSize, 32)}

		nextIP := copyIP(startIP)
		nextIP = incIP(nextIP, 1<<uint(32-requiredCIDRSize))

		if firstFitNet.Contains(nextIP) {
			remainderNets = append(remainderNets, &net.IPNet{
				IP:   copyIP(nextIP),
				Mask: net.CIDRMask(requiredCIDRSize, 32),
			})

		}

		for i := requiredCIDRSize; i > firstFitBits+1; i-- {
			nextTonextIP := copyIP(nextIP)

			nextTonextIP = incIP(nextTonextIP, 1<<uint(32-i))

			copy(nextIP, nextTonextIP)
			if firstFitNet.Contains(nextTonextIP) {
				remainderNets = append(remainderNets, &net.IPNet{
					IP:   copyIP(nextTonextIP),
					Mask: net.CIDRMask(i-1, 32),
				})

			}
		}
	} else if firstFitBits == requiredCIDRSize { // Exact fit
		allocatedNet = &net.IPNet{IP: copyIP(firstFitNet.IP), Mask: firstFitNet.Mask}
	}

	before := make([]*net.IPNet, 0, firstFitIndex)
	before = append(before, pool.FreeBlocks[:firstFitIndex]...)

	after := make([]*net.IPNet, 0, len(pool.FreeBlocks)-(firstFitIndex+1))
	if firstFitIndex+1 < len(pool.FreeBlocks) {
		after = append(after, pool.FreeBlocks[firstFitIndex+1:]...)
	}

	remainderCopy := make([]*net.IPNet, 0, len(remainderNets))
	for _, r := range remainderNets {
		if r == nil {
			continue
		}

		ipCp := copyIP(r.IP)
		maskCp := append(net.IPMask(nil), r.Mask...)
		remainderCopy = append(remainderCopy, &net.IPNet{
			IP:   ipCp,
			Mask: maskCp,
		})
	}

	newFree := make([]*net.IPNet, 0, len(before)+len(remainderCopy)+len(after))
	newFree = append(newFree, before...)
	newFree = append(newFree, remainderCopy...)
	newFree = append(newFree, after...)

	pool.FreeBlocks = newFree

	pool.Allocated[clusterName] = &net.IPNet{
		IP:   copyIP(allocatedNet.IP),
		Mask: append(net.IPMask(nil), allocatedNet.Mask...),
	}

	return allocatedNet, nil
}

func compareIPs(a, b net.IP) int {

	a4 := a.To4()
	b4 := b.To4()

	if a4 != nil && b4 != nil {

		for i := 0; i < net.IPv4len; i++ {
			if a4[i] < b4[i] {
				return -1
			}
			if a4[i] > b4[i] {
				return 1
			}
		}
		return 0
	}

	if a4 == nil && b4 == nil {

		for i := 0; i < net.IPv6len; i++ {
			if a[i] < b[i] {
				return -1
			}
			if a[i] > b[i] {
				return 1
			}
		}
		return 0
	}

	if a4 != nil && b4 == nil {
		return -1
	}
	if a4 == nil && b4 != nil {
		return 1
	}
	return 0
}
func compareIPNets(a, b *net.IPNet) int {
	cmp := compareIPs(a.IP, b.IP)
	if cmp != 0 {
		return cmp
	}
	onesA, _ := a.Mask.Size()
	onesB, _ := b.Mask.Size()
	bitsA := onesA
	bitsB := onesB
	if bitsA < bitsB {
		return 1
	}
	if bitsA > bitsB {
		return -1
	}
	return 0
}

func tryMerge(a, b *net.IPNet) (*net.IPNet, bool) {

	if a.IP.To4() == nil || b.IP.To4() == nil {
		return nil, false
	}

	bitsA, _ := a.Mask.Size()
	bitsB, _ := b.Mask.Size()
	if bitsA != bitsB {
		return nil, false
	}

	mergedBits := bitsA - 1

	if mergedBits < 0 {
		return nil, false
	}

	mergedMask := net.CIDRMask(mergedBits, 32)

	potentialMergedNet := &net.IPNet{IP: a.IP, Mask: mergedMask}

	blockSize := 1 << uint(32-bitsA)

	expectedNextIP := copyIP(a.IP)
	expectedNextIP = incIP(expectedNextIP, blockSize)

	if expectedNextIP.Equal(b.IP) {

		return potentialMergedNet, true
	}

	return nil, false
}

func incIP(ip net.IP, inc int) net.IP {

	res := copyIP(ip)

	carry := inc
	for i := len(res) - 1; i >= 0; i-- {
		if carry == 0 {
			break
		}
		sum := int(res[i]) + carry
		res[i] = byte(sum % 256)
		carry = sum / 256
	}
	return res
}
