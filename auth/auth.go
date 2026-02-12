package auth

type Checker struct {
	allowedUsers map[int64]bool
}

func New(userIDs []int64) *Checker {
	m := make(map[int64]bool, len(userIDs))
	for _, id := range userIDs {
		m[id] = true
	}
	return &Checker{allowedUsers: m}
}

func (c *Checker) IsAllowed(userID int64) bool {
	return c.allowedUsers[userID]
}
