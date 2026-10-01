package ingress

// fingerprintVector is recomputed independently by the control plane
// (same canonical policy, same digest).
const fingerprintVector = "044df9adb185271c19f095da2cd51208ca4792cf3ee0e728794dedbd70142018"

// fingerprintVectorCompat covers an IPv4-compatible IPv6 source written in
// upper case: both sides must store it as ::c000:200/120.
const fingerprintVectorCompat = "ef8c4f0691de9c367c790fb9fe0edf6822311cb2e0cdad88988d3e31a08ee918"
