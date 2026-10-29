package ferry

type buildManager struct{}

func (b *buildManager) Close() {}

func (e *Engine) initBuilder() {}

type snapshotRunner struct{}

func (s *snapshotRunner) Close() {}

func (e *Engine) initSnapshots() {}
