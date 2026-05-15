import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useDebouncedSearch } from './useDebouncedSearch'

describe('useDebouncedSearch', () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('debounces and calls the fetcher after delay when q ≥ minLen', async () => {
    const fetcher = vi.fn().mockResolvedValue([{ id: 1 }])
    const { result } = renderHook(() =>
      useDebouncedSearch({ delay: 250, minLen: 2, fetcher })
    )
    act(() => { result.current.onChange('ab') })
    expect(fetcher).not.toHaveBeenCalled()
    await act(async () => { vi.advanceTimersByTime(250) })
    expect(fetcher).toHaveBeenCalledWith('ab')
  })

  it('does not schedule any timer when fetcher is null, even with long input', async () => {
    const setTimeoutSpy = vi.spyOn(global, 'setTimeout')
    const { result } = renderHook(() =>
      useDebouncedSearch({ delay: 250, minLen: 2, fetcher: null })
    )
    setTimeoutSpy.mockClear()
    act(() => { result.current.onChange('long-query') })
    // No debounce timer should have been scheduled.
    const debounceCall = setTimeoutSpy.mock.calls.find(([, d]) => d === 250)
    expect(debounceCall).toBeUndefined()
    expect(result.current.results).toEqual([])
    expect(result.current.query).toBe('long-query')
    setTimeoutSpy.mockRestore()
  })

  it('resets state and clears any pending timer', async () => {
    const fetcher = vi.fn().mockResolvedValue([{ id: 1 }])
    const { result } = renderHook(() =>
      useDebouncedSearch({ delay: 250, minLen: 2, fetcher })
    )
    act(() => { result.current.onChange('ab') })
    act(() => { result.current.reset() })
    await act(async () => { vi.advanceTimersByTime(500) })
    expect(fetcher).not.toHaveBeenCalled()
    expect(result.current.query).toBe('')
  })
})
