package loop

// ManagedInputQueueCapacity is the maximum number of accepted managed inputs
// waiting behind active work. It is public because native and optional backends
// must share this observable boundary.
const ManagedInputQueueCapacity = 64
