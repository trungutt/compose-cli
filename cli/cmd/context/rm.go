/*
   Copyright 2020 Docker Compose CLI authors

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

package context

import (
	"github.com/docker/compose-cli/cli/mobycli"
	"github.com/spf13/cobra"
)

type removeOpts struct {
	force bool
}

func removeCommand() *cobra.Command {
	var opts removeOpts
	cmd := &cobra.Command{
		Use:     "rm CONTEXT [CONTEXT...]",
		Short:   "Remove one or more contexts",
		Aliases: []string{"remove"},
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mobycli.Exec(cmd.Root())
			return nil
		},
	}
	cmd.Flags().BoolVarP(&opts.force, "force", "f", false, "Force removing current context")

	return cmd
}
