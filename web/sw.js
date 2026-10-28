// Present because installability requires a registered service worker. It
// deliberately caches nothing: this page must always talk to the live API,
// and a stale cached shell would be worse than a slow load.
self.addEventListener('fetch', () => {});
