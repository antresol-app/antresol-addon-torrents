// setViewport: a copy of the core's web/src/test/setup.ts helper (the addon's
// tests reach no core sources).
/** Emulates a viewport width for components that use matchMedia. */
export function setViewport(width: number) {
  Object.defineProperty(window, 'innerWidth', { configurable: true, value: width })
  window.matchMedia = ((query: string) => {
    const max = /max-width:\s*(\d+)px/.exec(query)
    const min = /min-width:\s*(\d+)px/.exec(query)
    const reduce = /prefers-reduced-motion:\s*reduce/.test(query)
    let matches = false
    if (max) matches = width <= Number(max[1])
    else if (min) matches = width >= Number(min[1])
    else if (reduce) matches = false
    return {
      matches,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    } as MediaQueryList
  }) as typeof window.matchMedia
}
