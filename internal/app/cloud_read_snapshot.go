package app

// cloudReadSnapshotLocked constructs a read-only query context, not a copy of
// App's mutexes, pending authorizations or writers. Use only with query methods;
// mutations and preview-token registration must remain on the real App.
func (a *App) cloudReadSnapshotLocked() *App {
	return &App{opts: a.opts, state: clone(a.state), engine: a.engine,
		dnsClient: a.dnsClient, edgeClient: a.edgeClient, sslClient: a.sslClient,
		cloudflareClient: a.cloudflareClient, cloudflareResolve: a.cloudflareResolve, cloudflareProbe: a.cloudflareProbe,
		cloudflareTXTLookup: a.cloudflareTXTLookup,
		publicHost:          a.publicHost,
		zoneMemo:            append([]zoneEntry(nil), a.zoneMemo...), zoneMemoAt: a.zoneMemoAt,
		edgeMemo: append([]edgeSite(nil), a.edgeMemo...), edgeMemoAt: a.edgeMemoAt}
}
