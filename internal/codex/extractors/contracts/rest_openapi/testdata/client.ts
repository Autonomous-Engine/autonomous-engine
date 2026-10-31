async function getUsers() {
  const response = await fetch('/api/users');
  return response.json();
}

async function deleteUser(id: string) {
  await axios.delete('/api/users/' + id);
}
