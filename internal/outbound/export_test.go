package outbound

import "time"

// SetSigninWorkBudget shortens how long a sign-in tick may work.
func (s *Service) SetSigninWorkBudget(d time.Duration) { s.signinBudget = d }
