package service

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
)

// IPAMAllocator defines the interface for a dynamic IP address management system.
type IPAMAllocator interface {
	// InitializePool sets up a new IPAM pool for a given slice.
	// It takes the overall slice subnet and the *maximum* expected cluster CIDR size.
	InitializePool(sliceName, sliceSubnet string) error

	// Allocate assigns an available subnet of the required CIDR size to a cluster within a slice.
	// It is idempotent, returning the existing allocation if one is found.
	Allocate(ctx context.Context, sliceName string, clusterName string, requiredCIDRSize int) (string, error)

	// Reclaim returns a previously allocated subnet to the available pool.
	Reclaim(ctx context.Context, sliceName string, clusterName string) error
}

// sliceIPPool holds the state for a single slice's IPAM.
type sliceIPPool struct {
	// The overall CIDR block for the slice (e.g., "10.1.0.0/16").
	SliceSubnet *net.IPNet

	// Mutex to protect concurrent access to this pool's state.
	mu sync.Mutex

	// AllocatedBlocks tracks subnets that are currently in use.
	// Map: clusterName -> allocatedSubnetCIDR (as *net.IPNet).
	Allocated map[string]*net.IPNet

	// FreeBlocks is a list of available CIDR blocks.
	// It's kept sorted by network address for easier merging and searching.
	FreeBlocks []*net.IPNet
}

// DynamicIPAMAllocator implements the IPAMAllocator interface using in-memory storage.
// In a production environment, this state would be persisted (e.g., in a CRD).
type DynamicIPAMAllocator struct {
	// Mutex to protect the top-level pools map.
	mu sync.Mutex
	// Map: sliceName -> *sliceIPPool.
	pools map[string]*sliceIPPool
}

// NewDynamicIPAMAllocator creates and returns a new instance of the allocator.
func NewDynamicIPAMAllocator() *DynamicIPAMAllocator {
	return &DynamicIPAMAllocator{
		pools: make(map[string]*sliceIPPool),
	}
}

// InitializePool sets up the IPAM pool for a given slice.
// It parses the sliceSubnet and adds it as the initial free block.
func (a *DynamicIPAMAllocator) InitializePool(sliceName, sliceSubnetStr string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// If the pool already exists, no need to re-initialize.
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
	// Reserve a /24 subnet for VPN usage within the slice.
	vpnSubnetRequiredSize := 24 //Allocate the VPN subnet with a /24 size

	_, err = a.Allocate(context.Background(), sliceName, "KubeSlice-VPN-Reserved-Subnet", vpnSubnetRequiredSize)
	if err != nil {
		return fmt.Errorf("failed to reserve VPN subnet for slice %s: %w", sliceName, err)
	}

	return nil
}

// Allocate assigns a subnet of the required CIDR size to a cluster from the specified slice's pool.
// It is thread-safe and idempotent.
func (a *DynamicIPAMAllocator) Allocate(ctx context.Context, sliceName string, clusterName string, requiredCIDRSize int) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, exists := a.pools[sliceName]
	if !exists {
		return "", fmt.Errorf("ipam pool for slice %s is not initialized", sliceName)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	// 1. Check if the cluster already has an allocation (idempotency).
	if allocatedNet, found := pool.Allocated[clusterName]; found {
		// If the existing allocation is the correct size, return it.
		// If the size is different, this might indicate a configuration change.
		// For simplicity in this PR, we assume the requested size matches the existing.
		// A more robust system might re-allocate if sizes differ.
		_, existingBits := allocatedNet.Mask.Size()
		if existingBits == requiredCIDRSize {
			return allocatedNet.String(), nil
		}
		// If size differs, we need to reclaim and re-allocate.
		// This is a complex edge case for a first PR, so we'll treat it as an error for now.
		return "", fmt.Errorf("cluster %s already has subnet %s (/%d), but requested /%d. Re-allocation not supported in this version.",
			clusterName, allocatedNet.String(), existingBits, requiredCIDRSize)
	}

	// 2. Find a suitable free block.
	// We'll use a first-fit approach.
	var bestFitIndex = -1
	var bestFitNet *net.IPNet

	for i, freeNet := range pool.FreeBlocks {
		_, freeBits := freeNet.Mask.Size()
		if freeBits <= requiredCIDRSize { // If free block is larger or equal to requested size
			bestFitIndex = i
			bestFitNet = freeNet
			break // Found first fit
		}
	}

	if bestFitIndex == -1 {
		return "", fmt.Errorf("no available subnet of size /%d in pool for slice %s", requiredCIDRSize, sliceName)
	}

	// 4. Split the block if it's larger than needed.
	_, bestFitBits := bestFitNet.Mask.Size()
	var allocatedNet *net.IPNet
	remainderNets := []*net.IPNet{}

	if bestFitBits < requiredCIDRSize {

		allocatedNet = &net.IPNet{IP: bestFitNet.IP, Mask: net.CIDRMask(requiredCIDRSize, 32)}

		nextIP := make(net.IP, len(allocatedNet.IP)) // Use correct IP length
		copy(nextIP, allocatedNet.IP)
		incIP(nextIP, 1<<uint(32-requiredCIDRSize)) // Increment by the size of the allocated block

		// Create a new IPNet for the remainder
		if bestFitNet.Contains(nextIP) {
			// Ensure the nextIP is still within the original bestFitNet
			remainderNets = append(remainderNets, &net.IPNet{IP: nextIP, Mask: net.CIDRMask(requiredCIDRSize, 32)})
		}
		// Try to create further subnets for the remaining space
		for i := requiredCIDRSize; i > bestFitBits-1; i-- {
			nextTonextIP := make(net.IP, len(nextIP)) // Use correct IP length
			copy(nextTonextIP, nextIP)
			incIP(nextTonextIP, 1<<uint(32-i))
			copy(nextIP, nextTonextIP) // Update nextIP for the next iteration
			if bestFitNet.Contains(nextTonextIP) {
				// Ensure the nextIP is still within the original bestFitNet
				remainderNets = append(remainderNets, &net.IPNet{IP: nextTonextIP, Mask: net.CIDRMask(i, 32)})
			}
		}
	} else if bestFitBits == requiredCIDRSize { // Exact fit
		allocatedNet = bestFitNet
	} else { // Exact fit
		allocatedNet = bestFitNet
	}

	before := pool.FreeBlocks[:bestFitIndex]
	after := pool.FreeBlocks[bestFitIndex:]
	pool.FreeBlocks = append(append(before, remainderNets...), after...)

	// 5. Store the new allocation.
	pool.Allocated[clusterName] = allocatedNet

	return allocatedNet.String(), nil
}

// Reclaim returns a subnet to the available pool for a specified cluster.
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

	// 1. Check if the cluster actually has a subnet to reclaim.
	subnetToReclaim, allocated := pool.Allocated[clusterName]
	if !allocated {
		return fmt.Errorf("cluster %s has no allocated subnet in slice %s to reclaim", clusterName, sliceName)
	}

	// 2. Remove the allocation from the map.
	delete(pool.Allocated, clusterName)

	// 3. Add the reclaimed subnet back to the free list.
	pool.FreeBlocks = append(pool.FreeBlocks, subnetToReclaim)

	// 4. Attempt to merge adjacent free blocks (basic merge logic).
	// Sort the free blocks to make merging easier.
	sort.Slice(pool.FreeBlocks, func(i, j int) bool {
		return compareIPNets(pool.FreeBlocks[i], pool.FreeBlocks[j]) < 0
	})

	// Iterate and merge
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

// compareIPNets compares two IPNet objects for sorting.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Compares by IP address first, then by mask size.
func compareIPNets(a, b *net.IPNet) int {
	cmp := a.IP.Compare(b.IP)
	if cmp != 0 {
		return cmp
	}
	_, bitsA := a.Mask.Size()
	_, bitsB := b.Mask.Size()
	if bitsA < bitsB {
		return -1
	}
	if bitsA > bitsB {
		return 1
	}
	return 0
}

// tryMerge attempts to merge two adjacent CIDR blocks into a larger one.
// Returns the merged block and true if successful, otherwise nil and false.
// This is a simplified merge that only works for perfect binary merges (e.g., /24 + /24 -> /23).
func tryMerge(a, b *net.IPNet) (*net.IPNet, bool) {
	// Must be IPv4
	if a.IP.To4() == nil || b.IP.To4() == nil {
		return nil, false
	}

	// Must have same mask size
	_, bitsA := a.Mask.Size()
	_, bitsB := b.Mask.Size()
	if bitsA != bitsB {
		return nil, false
	}

	// Calculate the potential merged CIDR size (one bit smaller)
	mergedBits := bitsA - 1
	if mergedBits < 0 { // Can't merge smaller than /0
		return nil, false
	}

	// Calculate the network address of the potential merged block
	mergedMask := net.CIDRMask(mergedBits, 32)
	mergedIP := a.IP.Mask(mergedMask) // Masking 'a's IP with the new mask

	// Check if both original blocks are contained within this potential merged block
	potentialMergedNet := &net.IPNet{IP: mergedIP, Mask: mergedMask}
	if potentialMergedNet.Contains(a.IP) && potentialMergedNet.Contains(b.IP) {
		// Check if they are the two direct sub-blocks of the potential merged block
		// This is the tricky part: they must be exactly the two halves.
		// A simpler check: the second block's IP must be the first block's IP + (size of block)

		// Calculate the size of a single block (e.g., for /24, it's 256 IPs)
		blockSize := 1 << uint(32-bitsA)

		// Calculate the expected IP of the second block if it were the direct successor
		expectedNextIP := make(net.IP, len(a.IP))
		copy(expectedNextIP, a.IP)
		incIP(expectedNextIP, blockSize)

		if expectedNextIP.Equal(b.IP) {
			return potentialMergedNet, true
		}
	}

	return nil, false
}

// incIP increments an IP address by a given amount.
func incIP(ip net.IP, inc int) {
	carry := inc
	for i := len(ip) - 1; i >= 0; i-- {
		if carry == 0 {
			break
		}
		sum := int(ip[i]) + carry
		ip[i] = byte(sum % 256)

		// The new carry for the next (more significant) byte is the sum divided by 256
		carry = sum / 256

	}
	inc >>= 8
}
