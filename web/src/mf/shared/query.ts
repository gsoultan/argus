// `@tanstack/react-query`, in a section's bundle: the console's copy, so the
// section's queries live in the console's cache.
import { shell } from './shell'

export const { queryOptions, useMutation, useQuery, useQueryClient } = shell.query
