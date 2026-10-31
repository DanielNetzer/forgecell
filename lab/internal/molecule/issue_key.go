package molecule

import "strconv"

// DisplayKey is how the Issue is shown to the operator: the tracker's own Key
// when it set one, otherwise GitHub's "#N".
func (i Issue) DisplayKey() string {
	if i.Key != "" {
		return i.Key
	}
	return "#" + strconv.FormatInt(i.Number, 10)
}
