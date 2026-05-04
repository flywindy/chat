import { useState, useRef, useEffect } from 'react'

export function useDebouncedSearch({ delay = 250, minLen = 2, fetcher }) {
  const [query, setQuery] = useState('')
  const [results, setResults] = useState([])
  const [loading, setLoading] = useState(false)
  const debounceRef = useRef(null)

  useEffect(() => () => clearTimeout(debounceRef.current), [])

  const onChange = (q) => {
    setQuery(q)
    clearTimeout(debounceRef.current)
    if (q.length < minLen) {
      setResults([])
      setLoading(false)
      return
    }
    debounceRef.current = setTimeout(async () => {
      setLoading(true)
      try {
        const resp = await fetcher(q)
        setResults(resp ?? [])
      } catch {
        setResults([])
      } finally {
        setLoading(false)
      }
    }, delay)
  }

  const reset = () => {
    setQuery('')
    setResults([])
    setLoading(false)
    clearTimeout(debounceRef.current)
  }

  return { query, results, loading, onChange, reset }
}
