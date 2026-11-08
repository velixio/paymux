package outbound

import "context"

// SetSigninWorkContext replaces how a sign-in tick's work context is made; nil
// restores the time limit.
func (s *Service) SetSigninWorkContext(fn func(context.Context) (context.Context, context.CancelFunc)) {
	s.signinWorkCtx = fn
}
