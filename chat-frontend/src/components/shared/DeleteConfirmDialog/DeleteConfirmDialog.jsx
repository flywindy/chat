import Modal from '../Modal/Modal'

export default function DeleteConfirmDialog({ onConfirm, onCancel, pending }) {
  const headingId = 'delete-confirm-heading'
  return (
    <Modal onClose={onCancel} labelledBy={headingId}>
      <h2 id={headingId}>Delete message?</h2>
      <p>This cannot be undone.</p>
      <div className="dialog-actions">
        <button type="button" className="dialog-cancel" onClick={onCancel} disabled={pending}>Cancel</button>
        <button type="button" onClick={onConfirm} disabled={pending}>
          {pending ? 'Deleting…' : 'Delete'}
        </button>
      </div>
    </Modal>
  )
}
