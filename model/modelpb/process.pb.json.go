// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package modelpb

import "github.com/elastic/apm-data/model/internal/modeljson"

func (p *Process) toModelJSON(out *modeljson.Process) {
	*out = modeljson.Process{
		Pid:         int(p.Pid),
		Title:       p.Title,
		CommandLine: p.CommandLine,
		Executable:  p.Executable,
		Args:        p.Argv,
		Parent: modeljson.ProcessParent{
			Pid: p.Ppid,
		},
	}
	if p.Thread != nil {
		out.Thread = modeljson.ProcessThread{
			Name: p.Thread.Name,
			ID:   int(p.Thread.Id),
		}
	}
}
