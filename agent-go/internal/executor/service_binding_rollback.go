package executor

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// Image rollback may restore ordinary application settings, but must not switch
// the dedicated database login independently of its reviewed binding lifecycle.
func serviceBindingRollbackCompatible(current, target []byte) error {
	read := func(raw []byte) (map[string]string, error) {
		var model struct {
			Services map[string]struct {
				Environment map[string]any             `json:"environment"`
				Networks    map[string]json.RawMessage `json:"networks"`
			} `json:"services"`
		}
		if json.Unmarshal(raw, &model) != nil || len(model.Services) == 0 {
			return nil, errors.New("cannot verify release database connections")
		}
		connections := map[string]string{}
		for name, service := range model.Services {
			bound := false
			for network := range service.Networks {
				bound = bound || strings.HasPrefix(network, "impreza-binding-")
			}
			value, _ := service.Environment["DATABASE_URL"].(string)
			managed := bindingRuntimeURLPattern.MatchString("DATABASE_URL=" + value)
			if bound && !managed {
				return nil, errors.New("release database connection cannot be verified")
			}
			if managed {
				connections[name] = value
			}
		}
		return connections, nil
	}
	a, err := read(current)
	if err != nil {
		return err
	}
	b, err := read(target)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return errors.New("release changes a managed database connection; review the connection before redeploying; containers were not changed")
	}
	return nil
}
