import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import Sidebar from './Sidebar'

vi.mock('./RoomList/RoomList', () => ({
  default: ({ selectedRoomId, onSelectRoom }) => (
    <button type="button" onClick={() => onSelectRoom({ id: 'r1' })}>
      RoomList:{selectedRoomId ?? 'none'}
    </button>
  ),
}))
vi.mock('./CreateRoomDialog/CreateRoomDialog', () => ({
  default: ({ onClose, onCreated }) => (
    <div role="dialog">
      <button type="button" onClick={onClose}>close-create</button>
      <button type="button" onClick={() => onCreated({ id: 'new-room' })}>did-create</button>
    </div>
  ),
}))

describe('Sidebar', () => {
  it('renders the room list with selectedRoomId and forwards onSelectRoom', () => {
    const onSelectRoom = vi.fn()
    render(<Sidebar selectedRoomId="r-current" onSelectRoom={onSelectRoom} />)
    fireEvent.click(screen.getByText('RoomList:r-current'))
    expect(onSelectRoom).toHaveBeenCalledWith({ id: 'r1' })
  })

  // CreateRoomDialog is lazy-loaded — fireEvent.click triggers the
  // import but the dialog only mounts after the promise resolves, so
  // the assertions below use findByRole/findByText to await it.
  it('opens CreateRoomDialog when "+ Create Room" is clicked, closes via dialog', async () => {
    render(<Sidebar selectedRoomId={null} onSelectRoom={() => {}} />)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /create room/i }))
    expect(await screen.findByRole('dialog')).toBeInTheDocument()
    fireEvent.click(screen.getByText('close-create'))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('after CreateRoomDialog reports onCreated, dispatches onSelectRoom with the new room', async () => {
    const onSelectRoom = vi.fn()
    render(<Sidebar selectedRoomId={null} onSelectRoom={onSelectRoom} />)
    fireEvent.click(screen.getByRole('button', { name: /create room/i }))
    fireEvent.click(await screen.findByText('did-create'))
    expect(onSelectRoom).toHaveBeenCalledWith({ id: 'new-room' })
  })
})
