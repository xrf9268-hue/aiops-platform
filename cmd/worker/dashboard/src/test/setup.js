import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

const localStorageStore = new Map();
Object.defineProperty(window, 'localStorage', {
  configurable: true,
  value: {
    getItem(key) {
      return localStorageStore.has(key) ? localStorageStore.get(key) : null;
    },
    setItem(key, value) {
      localStorageStore.set(key, String(value));
    },
    removeItem(key) {
      localStorageStore.delete(key);
    },
    clear() {
      localStorageStore.clear();
    },
  },
});

afterEach(() => {
  cleanup();
  localStorageStore.clear();
});

// jsdom does not implement matchMedia, which useTheme relies on for the
// system-preference default.
if (!window.matchMedia) {
  window.matchMedia = (query) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener() {},
    removeEventListener() {},
    addListener() {},
    removeListener() {},
    dispatchEvent() {
      return false;
    },
  });
}
