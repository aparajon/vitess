/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endtoend

import (
	"testing"

	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/vt/vttablet/endtoend/framework"
)

var procSQL = []string{`create procedure proc_select1()
BEGIN
	select intval from vitess_test;
END;`, `create procedure proc_select4()
BEGIN
	select intval from vitess_test;
	select intval from vitess_test;
	select intval from vitess_test;
	select intval from vitess_test;
END;`, `create procedure proc_dml()
BEGIN
    start transaction;
	insert into vitess_test(intval) values(1432);
	update vitess_test set intval = 2341 where intval = 1432;
	delete from vitess_test where intval = 2341;
    commit;
END;`}

func TestCallProcedure(t *testing.T) {
	client := framework.NewClient()
	type testcases struct {
		query   string
		wantErr bool
	}
	tcases := []testcases{{
		query:   "call proc_select1()",
		wantErr: true,
	}, {
		query:   "call proc_select4()",
		wantErr: true,
	}, {
		query: "call proc_dml()",
	}}

	for _, tc := range tcases {
		t.Run(tc.query, func(t *testing.T) {
			_, err := client.Execute(tc.query, nil)
			if tc.wantErr {
				require.EqualError(t, err, "Multi-Resultset not supported in stored procedure (CallerID: dev)")
				return
			}
			require.NoError(t, err)

		})
	}
}
