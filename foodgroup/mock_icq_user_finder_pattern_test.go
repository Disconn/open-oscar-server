// Hand-written extension for mockICQUserFinder.
//
// FindByICQNamePattern isn't part of the ICQUserFinder interface itself – the
// service probes for it via an unexported `icqNamePatternFinder` type
// assertion. Production code hits that assertion through *state.SQLiteUserStore,
// which already implements the method; tests opt in by compiling this
// extension, which gives the generated mockICQUserFinder the same shape.

package foodgroup

import (
	"context"

	"github.com/mk6i/open-oscar-server/state"
	mock "github.com/stretchr/testify/mock"
)

// FindByICQNamePattern mirrors (*state.SQLiteUserStore).FindByICQNamePattern on
// the generated mock so it also satisfies icqNamePatternFinder.
func (_mock *mockICQUserFinder) FindByICQNamePattern(ctx context.Context, firstPat string, lastPat string, nickPat string) ([]state.User, error) {
	ret := _mock.Called(ctx, firstPat, lastPat, nickPat)

	if len(ret) == 0 {
		panic("no return value specified for FindByICQNamePattern")
	}

	var r0 []state.User
	var r1 error
	if returnFunc, ok := ret.Get(0).(func(context.Context, string, string, string) ([]state.User, error)); ok {
		return returnFunc(ctx, firstPat, lastPat, nickPat)
	}
	if returnFunc, ok := ret.Get(0).(func(context.Context, string, string, string) []state.User); ok {
		r0 = returnFunc(ctx, firstPat, lastPat, nickPat)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).([]state.User)
		}
	}
	if returnFunc, ok := ret.Get(1).(func(context.Context, string, string, string) error); ok {
		r1 = returnFunc(ctx, firstPat, lastPat, nickPat)
	} else {
		r1 = ret.Error(1)
	}
	return r0, r1
}

// mockICQUserFinder_FindByICQNamePattern_Call is a *mock.Call that shadows
// Run/Return methods for 'FindByICQNamePattern'.
type mockICQUserFinder_FindByICQNamePattern_Call struct {
	*mock.Call
}

// FindByICQNamePattern is a helper method to define mock.On call
//   - ctx context.Context
//   - firstPat string
//   - lastPat string
//   - nickPat string
func (_e *mockICQUserFinder_Expecter) FindByICQNamePattern(ctx interface{}, firstPat interface{}, lastPat interface{}, nickPat interface{}) *mockICQUserFinder_FindByICQNamePattern_Call {
	return &mockICQUserFinder_FindByICQNamePattern_Call{Call: _e.mock.On("FindByICQNamePattern", ctx, firstPat, lastPat, nickPat)}
}

func (_c *mockICQUserFinder_FindByICQNamePattern_Call) Run(run func(ctx context.Context, firstPat string, lastPat string, nickPat string)) *mockICQUserFinder_FindByICQNamePattern_Call {
	_c.Call.Run(func(args mock.Arguments) {
		var arg0 context.Context
		if args[0] != nil {
			arg0 = args[0].(context.Context)
		}
		var arg1 string
		if args[1] != nil {
			arg1 = args[1].(string)
		}
		var arg2 string
		if args[2] != nil {
			arg2 = args[2].(string)
		}
		var arg3 string
		if args[3] != nil {
			arg3 = args[3].(string)
		}
		run(arg0, arg1, arg2, arg3)
	})
	return _c
}

func (_c *mockICQUserFinder_FindByICQNamePattern_Call) Return(users []state.User, err error) *mockICQUserFinder_FindByICQNamePattern_Call {
	_c.Call.Return(users, err)
	return _c
}

func (_c *mockICQUserFinder_FindByICQNamePattern_Call) RunAndReturn(run func(ctx context.Context, firstPat string, lastPat string, nickPat string) ([]state.User, error)) *mockICQUserFinder_FindByICQNamePattern_Call {
	_c.Call.Return(run)
	return _c
}
