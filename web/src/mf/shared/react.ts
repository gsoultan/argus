// `react`, in a section's bundle: the console's copy (see ../contract.ts).
import { shell } from './shell'

const R = shell.React
export default R
export const {
  Children, Component, Fragment, PureComponent, StrictMode, Suspense, cloneElement, createContext, createElement,
  forwardRef, isValidElement, lazy, memo, startTransition, use, useCallback, useContext, useDebugValue,
  useDeferredValue, useEffect, useId, useImperativeHandle, useInsertionEffect, useLayoutEffect, useMemo,
  useReducer, useRef, useState, useSyncExternalStore, useTransition, version,
} = R
