import {
  Alert, Anchor, Badge, Box, Button, Checkbox, Code, CopyButton, Group, Modal, NumberInput, SegmentedControl, Select, Stack, Switch, Table, Tabs, Text, TextInput, Textarea,
} from '@mantine/core'

/*
 * The Mantine components the console lends a section (contract.ts): exactly
 * the ones the sections built for this contract use (mf/shared/mantine.ts,
 * which the compiler checks against this type). Lending more is not free:
 * each one ties that component's chunk to the section's route, and Rollup
 * then splits it out of the chunks the overview already loads, which put
 * the first visit over its weight budget (e2e/weight.spec.ts). A section
 * needing one more is built for the contract version that lends it.
 */
export const LENT_MANTINE = {
  Alert, Anchor, Badge, Box, Button, Checkbox, Code, CopyButton, Group, Modal, NumberInput, SegmentedControl, Select, Stack, Switch, Table, Tabs, Text, TextInput, Textarea,
}

export type LentMantine = typeof LENT_MANTINE
