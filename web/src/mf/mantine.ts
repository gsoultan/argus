import {
  Accordion, ActionIcon, Alert, Anchor, Badge, Box, Button, Card, Center, Checkbox, Chip, CloseButton, Code, Collapse, CopyButton, Divider, Drawer, Flex, Grid, Group, Indicator, Kbd, Loader, Menu, Modal, MultiSelect, NumberInput, Pagination, Paper, PasswordInput, Pill, Popover, Progress, Radio, ScrollArea, SegmentedControl, Select, SimpleGrid, Skeleton, Space, Stack, Stepper, Switch, Table, Tabs, TagsInput, Text, TextInput, Textarea, ThemeIcon, Title, Tooltip, UnstyledButton,
} from '@mantine/core'

/*
 * The Mantine components the console lends a section (contract.ts). Named
 * rather than the whole library, so lending them costs the console only these;
 * a section needing one more is built for the contract version that lends it.
 */
export const LENT_MANTINE = {
  Accordion, ActionIcon, Alert, Anchor, Badge, Box, Button, Card, Center, Checkbox, Chip, CloseButton, Code, Collapse, CopyButton, Divider, Drawer, Flex, Grid, Group, Indicator, Kbd, Loader, Menu, Modal, MultiSelect, NumberInput, Pagination, Paper, PasswordInput, Pill, Popover, Progress, Radio, ScrollArea, SegmentedControl, Select, SimpleGrid, Skeleton, Space, Stack, Stepper, Switch, Table, Tabs, TagsInput, Text, TextInput, Textarea, ThemeIcon, Title, Tooltip, UnstyledButton,
}

export type LentMantine = typeof LENT_MANTINE
