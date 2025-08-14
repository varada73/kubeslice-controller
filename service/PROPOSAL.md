# PROPOSAL: Dynamic IPAM Allocator for KubeSlice

## 📄 Introduction

This document outlines a proposal for a **Dynamic IPAM Allocator** (IP Address Management) designed to integrate with KubeSlice. The primary goal is to efficiently manage and allocate network subnets to individual clusters within a KubeSlice environment while minimizing IP address space fragmentation. The proposed solution is a self-contained Go package for dynamic subnet provisioning.

---

## 🎯 Problem Statement

In a multi-tenant, multi-cluster environment like KubeSlice, a central IPAM system is crucial for efficient and reliable network management. Key challenges include:

1.  **Dynamic Provisioning:** Clusters are created and destroyed dynamically, requiring a system that can allocate subnets on demand and reclaim them for future use.

2.  **Efficient Utilization:** Statically assigning large subnets leads to wasted IP space. A dynamic system ensures that the IP space is used effectively.

3.  **Fragmentation:** A series of allocations and reclamations can fragment the available IP space into many small, unusable blocks. This design must address fragmentation to maintain long-term efficiency.

4.  **Concurrency:** The system must be thread-safe to prevent race conditions and ensure data integrity when multiple clusters request subnets simultaneously.

---

## 💡 Proposed Solution: Dynamic IPAM Allocator

The proposed solution is a `DynamicIPAMAllocator` that manages a collection of `sliceIPPools`. Each `sliceIPPool` is responsible for a single KubeSlice and handles all subnet allocations and reclamations for the clusters within that slice. The core logic is built around a "first-fit" allocation strategy combined with an intelligent merging mechanism to reduce fragmentation.

### Core Logic and Data Structures

The system uses a `sliceIPPool` struct to track the state for each slice:

* **`SliceSubnet`**: The overall CIDR block for the entire slice.

* **`Allocated`**: A map that stores the subnets currently assigned to clusters, mapping the `clusterName` to the allocated `net.IPNet`. This prevents a cluster from being double-allocated.

* **`FreeBlocks`**: A sorted slice of `net.IPNet` objects representing the contiguous blocks of available IP space within the slice subnet. This is the heart of the allocator's logic.

**Allocation Process (`Allocate` method):**

1.  When a request to allocate a subnet of a specific size (`requiredCIDRSize`) is received, the allocator locks the corresponding slice pool to ensure concurrency safety.

2.  It iterates through the **`FreeBlocks`** slice, which is always kept sorted by IP address.

3.  It finds the **first available free block** that is large enough to contain the required subnet.

4.  If an exact-fit block is found, it is allocated and removed from the `FreeBlocks` list.

5.  If a larger block is found, the allocator carves out the required subnet from the beginning of that block. The remaining, unused portion is added back to the `FreeBlocks` list as a new, smaller free block.

6.  The new subnet is recorded in the `Allocated` map, and its CIDR string is returned.

**Reclamation Process (`Reclaim` method):**

1.  When a cluster is removed, its subnet is identified in the `Allocated` map and removed.

2.  The reclaimed subnet is then added back to the **`FreeBlocks`** slice.

3.  The `FreeBlocks` slice is re-sorted by IP address.

4.  A crucial step is a **merging algorithm**: the code iterates through the newly sorted `FreeBlocks` list and attempts to merge adjacent blocks. For example, a reclaimed `10.0.0.0/25` and an adjacent free `10.0.0.128/25` would be merged into a single `10.0.0.0/24` block. This is how the system actively reduces fragmentation.

---

## ✅ Key Features

* **Dynamic Subnet Allocation**: Supports on-demand allocation of subnets of a specified size.

* **Fragmentation Reduction**: The merging logic during reclamation ensures that free blocks are consolidated, preventing the IP space from becoming fragmented over time.

* **Concurrency Safety**: `sync.Mutex` is used to protect both the global allocator state and each individual slice pool's state, making the service safe for concurrent access.

* **Dedicated VPN Subnet**: The `InitializePool` method automatically reserves a specific subnet size (`/24` in the current implementation) for VPN services, ensuring that a dedicated network is always available for this critical function.

* **Idempotent Allocation**: The `allocateSubnetForPool` method checks if a cluster already has an allocated subnet. If a request for an existing cluster with the same subnet size is made, it returns the existing subnet without making any changes, making the operation safe to retry.

---

## 🚀 Next Steps

This proposal outlines a robust and efficient IPAM solution. The next steps would be to:

-   Integrate this service into the KubeSlice control plane.
-   Refine and expand the existing preliminary unit and integration tests to ensure all edge cases of allocation and reclamation are handled correctly, particularly for different CIDR sizes and merging scenarios.
-   Consider adding persistent storage (e.g., using Kubernetes Custom Resources or a database) for the IPAM state to make it resilient to service restarts.