// «Мои слежения» (adult profiles, from the profile menu): the follows this
// profile added; the admin manages all of them in «Управление → Слежение».
import { Follows } from './Follows'
import styles from './Follows.module.css'

export function MyFollows() {
  return (
    // the app draws the page's frame (contract: «routes»)
    <div className={styles.pageColumn}>
      <Follows mine />
    </div>
  )
}

export default MyFollows
